// Package s3 is a minimal client for S3-compatible object storage (AWS S3,
// Cloudflare R2, Backblaze B2, MinIO, RustFS, ...). It implements only what
// saba's backups need — put, get, delete, list — with AWS Signature V4 and
// multipart uploads for large objects, and has no dependencies.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned by Get for a missing object.
var ErrNotFound = errors.New("s3: object not found")

type Config struct {
	// Endpoint is the service URL, e.g. https://s3.eu-central-1.amazonaws.com
	// or https://<account>.r2.cloudflarestorage.com. Empty means AWS for Region.
	Endpoint string `json:"endpoint,omitempty"`
	Region   string `json:"region,omitempty"`
	Bucket   string `json:"bucket"`
	// Prefix is prepended to every object key ("saba/" for example).
	Prefix          string `json:"prefix,omitempty"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	// PathStyle forces https://endpoint/bucket/key addressing. It is the
	// default for every endpoint except AWS itself.
	PathStyle *bool `json:"path_style,omitempty"`
}

type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

type Client struct {
	cfg       Config
	base      *url.URL // scheme + host (+ bucket in the host for virtual-hosted style)
	pathStyle bool
	HTTP      *http.Client
	// PartSize is the multipart part size; objects up to this size use a
	// single request. The S3 minimum is 5 MiB.
	PartSize int64
	now      func() time.Time
}

func New(cfg Config) (*Client, error) {
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	if cfg.Bucket == "" {
		return nil, errors.New("a bucket is required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("an access key id and a secret access key are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if endpoint == "" {
		endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid endpoint %q", cfg.Endpoint)
	}
	pathStyle := !strings.HasSuffix(u.Hostname(), ".amazonaws.com")
	if cfg.PathStyle != nil {
		pathStyle = *cfg.PathStyle
	}
	if !pathStyle {
		u.Host = cfg.Bucket + "." + u.Host
	}
	if cfg.Prefix != "" {
		cfg.Prefix = strings.Trim(cfg.Prefix, "/") + "/"
	}
	return &Client{
		cfg: cfg, base: &url.URL{Scheme: u.Scheme, Host: u.Host}, pathStyle: pathStyle,
		HTTP:     &http.Client{Timeout: 0, Transport: http.DefaultTransport},
		PartSize: 64 << 20,
		now:      time.Now,
	}, nil
}

// uriEncode percent-encodes as Signature V4 requires.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// request builds and signs a request. key is the object key without the
// configured prefix; an empty key addresses the bucket itself.
func (c *Client) request(ctx context.Context, method, key string, query url.Values, body io.Reader, size int64, payloadHash string) (*http.Request, error) {
	path := "/"
	if c.pathStyle {
		path += c.cfg.Bucket
		if key != "" {
			path += "/"
		}
	}
	path += key
	escaped := uriEncode(path, true)

	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var q []string
	for _, k := range keys {
		q = append(q, uriEncode(k, false)+"="+uriEncode(query.Get(k), false))
	}
	rawQuery := strings.Join(q, "&")

	u := *c.base
	u.Path, u.RawPath, u.RawQuery = path, escaped, rawQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody // makes Go send an explicit "Content-Length: 0"
		}
	}
	now := c.now().UTC()
	amzDate, date := now.Format("20060102T150405Z"), now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	scope := date + "/" + c.cfg.Region + "/s3/aws4_request"
	canonical := strings.Join([]string{
		method, escaped, rawQuery,
		"host:" + u.Host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n",
		"host;x-amz-content-sha256;x-amz-date", payloadHash,
	}, "\n")
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashHex([]byte(canonical))
	k := hmacSHA256([]byte("AWS4"+c.cfg.SecretAccessKey), date)
	k = hmacSHA256(k, c.cfg.Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.cfg.AccessKeyID+"/"+scope+
		", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+hex.EncodeToString(hmacSHA256(k, toSign)))
	return req, nil
}

var emptyHash = hashHex(nil)

type apiError struct {
	Status  int
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (e *apiError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("s3: %s: %s", e.Code, strings.TrimSuffix(e.Message, "."))
	case e.Code != "":
		return "s3: " + e.Code
	}
	return fmt.Sprintf("s3: request failed with HTTP %d", e.Status)
}

// do sends a request and turns non-2xx responses into errors.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3: cannot reach %s: %w", c.base.Host, err)
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &apiError{Status: resp.StatusCode}
	_ = xml.Unmarshal(b, e)
	if resp.StatusCode == http.StatusNotFound && (e.Code == "NoSuchKey" || e.Code == "") && req.Method != http.MethodPut {
		return nil, ErrNotFound
	}
	return nil, e
}

func (c *Client) key(k string) string { return c.cfg.Prefix + k }

// sectionHash returns the SHA-256 of size bytes of f starting at off.
func sectionHash(f *os.File, off, size int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, off, size)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Put uploads a file. Large files are sent in parts so that neither side
// has to handle one huge request.
func (c *Client) Put(ctx context.Context, key string, f *os.File, size int64) error {
	key = c.key(key)
	if size <= c.PartSize {
		sum, err := sectionHash(f, 0, size)
		if err != nil {
			return err
		}
		req, err := c.request(ctx, http.MethodPut, key, nil, io.NewSectionReader(f, 0, size), size, sum)
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	return c.putMultipart(ctx, key, f, size)
}

func (c *Client) putMultipart(ctx context.Context, key string, f *os.File, size int64) (err error) {
	req, err := c.request(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, 0, emptyHash)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	var created struct {
		UploadID string `xml:"UploadId"`
	}
	derr := xml.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if derr != nil || created.UploadID == "" {
		return errors.New("s3: the service did not return an upload id")
	}
	id := url.Values{"uploadId": {created.UploadID}}
	defer func() {
		if err != nil { // do not leave billed, invisible parts behind
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if req, rerr := c.request(actx, http.MethodDelete, key, id, nil, 0, emptyHash); rerr == nil {
				if resp, derr := c.do(req); derr == nil {
					resp.Body.Close()
				}
			}
		}
	}()

	type part struct {
		PartNumber int
		ETag       string
	}
	var parts []part
	for off, n := int64(0), 1; off < size; off, n = off+c.PartSize, n+1 {
		length := min(c.PartSize, size-off)
		sum, err := sectionHash(f, off, length)
		if err != nil {
			return err
		}
		q := url.Values{"uploadId": {created.UploadID}, "partNumber": {strconv.Itoa(n)}}
		req, err := c.request(ctx, http.MethodPut, key, q, io.NewSectionReader(f, off, length), length, sum)
		if err != nil {
			return err
		}
		resp, err := c.do(req)
		if err != nil {
			return fmt.Errorf("part %d: %w", n, err)
		}
		resp.Body.Close()
		parts = append(parts, part{n, resp.Header.Get("ETag")})
	}
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Part    []part   `xml:"Part"`
	}{Part: parts})
	if err != nil {
		return err
	}
	if req, err = c.request(ctx, http.MethodPost, key, id, bytes.NewReader(body), int64(len(body)), hashHex(body)); err != nil {
		return err
	}
	if resp, err = c.do(req); err != nil {
		return err
	}
	// S3 may answer 200 with an error document for this call.
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if bytes.Contains(b, []byte("<Error>")) {
		e := &apiError{Status: resp.StatusCode}
		_ = xml.Unmarshal(b, e)
		return e
	}
	return nil
}

// Get opens an object for reading and returns its size.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	req, err := c.request(ctx, http.MethodGet, c.key(key), nil, nil, 0, emptyHash)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// Delete removes an object. Deleting a missing object is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	req, err := c.request(ctx, http.MethodDelete, c.key(key), nil, nil, 0, emptyHash)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// List returns the objects under prefix (relative to the configured prefix),
// with keys relative to the configured prefix as well.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {c.key(prefix)}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := c.request(ctx, http.MethodGet, "", q, nil, 0, emptyHash)
		if err != nil {
			return nil, err
		}
		resp, err := c.do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			IsTruncated bool   `xml:"IsTruncated"`
			Next        string `xml:"NextContinuationToken"`
			Contents    []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("s3: cannot parse the listing: %w", err)
		}
		for _, o := range page.Contents {
			out = append(out, Object{Key: strings.TrimPrefix(o.Key, c.cfg.Prefix), Size: o.Size, Modified: o.LastModified})
		}
		if !page.IsTruncated || page.Next == "" {
			return out, nil
		}
		token = page.Next
	}
}

// CreateBucket creates the configured bucket. An existing bucket owned by
// the same account is not an error.
func (c *Client) CreateBucket(ctx context.Context) error {
	var body []byte
	if c.cfg.Region != "us-east-1" && strings.HasSuffix(c.base.Hostname(), ".amazonaws.com") {
		body = []byte(`<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><LocationConstraint>` + c.cfg.Region + `</LocationConstraint></CreateBucketConfiguration>`)
	}
	req, err := c.bucketRequest(ctx, http.MethodPut, body)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Code == "BucketAlreadyOwnedByYou" || ae.Code == "BucketAlreadyExists") {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) bucketRequest(ctx context.Context, method string, body []byte) (*http.Request, error) {
	if body == nil {
		return c.request(ctx, method, "", nil, nil, 0, emptyHash)
	}
	return c.request(ctx, method, "", nil, bytes.NewReader(body), int64(len(body)), hashHex(body))
}

// Check verifies that the credentials can write, read back, list and delete
// in the bucket, and returns an error that says which step failed.
func (c *Client) Check(ctx context.Context) error {
	const probe = ".saba-access-check"
	content := []byte("saba " + c.now().UTC().Format(time.RFC3339))
	req, err := c.request(ctx, http.MethodPut, c.key(probe), nil, bytes.NewReader(content), int64(len(content)), hashHex(content))
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("cannot write to the bucket: %w", err)
	}
	resp.Body.Close()
	body, _, err := c.Get(ctx, probe)
	if err != nil {
		return fmt.Errorf("cannot read from the bucket: %w", err)
	}
	got, _ := io.ReadAll(io.LimitReader(body, 1024))
	body.Close()
	if !bytes.Equal(got, content) {
		return errors.New("the bucket returned different data than was written")
	}
	if _, err := c.List(ctx, probe); err != nil {
		return fmt.Errorf("cannot list the bucket: %w", err)
	}
	if err := c.Delete(ctx, probe); err != nil {
		return fmt.Errorf("cannot delete from the bucket: %w", err)
	}
	return nil
}
