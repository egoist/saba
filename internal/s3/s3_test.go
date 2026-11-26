package s3

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run against a real S3-compatible server. Start one (for example
// `docker run -p 9000:9000 rustfs/rustfs`) and set SABA_TEST_S3_ENDPOINT,
// SABA_TEST_S3_ACCESS_KEY and SABA_TEST_S3_SECRET_KEY to enable them.
func testClient(t *testing.T) *Client {
	t.Helper()
	endpoint := os.Getenv("SABA_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("SABA_TEST_S3_ENDPOINT is not set")
	}
	c, err := New(Config{
		Endpoint: endpoint, Bucket: "saba-test", Prefix: "unit/" + time.Now().Format("150405.000"),
		AccessKeyID: os.Getenv("SABA_TEST_S3_ACCESS_KEY"), SecretAccessKey: os.Getenv("SABA_TEST_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CreateBucket(context.Background()); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return c
}

func tempFile(t *testing.T, size int) (*os.File, [32]byte) {
	t.Helper()
	data := make([]byte, size)
	rand.Read(data)
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f, sha256.Sum256(data)
}

func TestAgainstRealServer(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if err := c.Check(ctx); err != nil {
		t.Fatalf("access check: %v", err)
	}

	fetch := func(key string) [32]byte {
		t.Helper()
		body, size, err := c.Get(ctx, key)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		defer body.Close()
		h := sha256.New()
		n, _ := io.Copy(h, body)
		if size >= 0 && n != size {
			t.Fatalf("get %s: read %d bytes, Content-Length said %d", key, n, size)
		}
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		return sum
	}

	// Single request upload, with a key that needs careful URL encoding.
	small, smallSum := tempFile(t, 200_000)
	const awkward = "main/2026 backup+final (1)/ünï©ode=&.sql.gz"
	if err := c.Put(ctx, awkward, small, 200_000); err != nil {
		t.Fatalf("put: %v", err)
	}
	if fetch(awkward) != smallSum {
		t.Fatal("downloaded object differs from the upload")
	}

	// Multipart upload: three parts, the last one short.
	c.PartSize = 5 << 20
	const bigSize = 12<<20 + 12345
	big, bigSum := tempFile(t, bigSize)
	if err := c.Put(ctx, "main/big.sql.gz", big, bigSize); err != nil {
		t.Fatalf("multipart put: %v", err)
	}
	if fetch("main/big.sql.gz") != bigSum {
		t.Fatal("multipart object differs from the upload")
	}
	empty, _ := tempFile(t, 0)
	if err := c.Put(ctx, "other/empty", empty, 0); err != nil {
		t.Fatalf("empty put: %v", err)
	}

	// Listing is scoped to the prefix and returns keys relative to it.
	objs, err := c.List(ctx, "main/")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int64{}
	for _, o := range objs {
		found[o.Key] = o.Size
	}
	if len(found) != 2 || found[awkward] != 200_000 || found["main/big.sql.gz"] != bigSize {
		t.Fatalf("listing: %v", found)
	}
	if objs[0].Modified.IsZero() || time.Since(objs[0].Modified) > time.Hour {
		t.Fatalf("listing has no usable modification time: %v", objs[0].Modified)
	}

	// Deletes are idempotent; reading a missing object is a distinct error.
	for i := 0; i < 2; i++ {
		if err := c.Delete(ctx, awkward); err != nil {
			t.Fatalf("delete #%d: %v", i+1, err)
		}
	}
	if _, _, err := c.Get(ctx, awkward); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	for _, k := range []string{"main/big.sql.gz", "other/empty"} {
		_ = c.Delete(ctx, k)
	}
	if objs, _ := c.List(ctx, ""); len(objs) != 0 {
		t.Fatalf("objects left behind: %v", objs)
	}

	// Wrong credentials and a missing bucket are reported as such.
	bad, _ := New(Config{Endpoint: os.Getenv("SABA_TEST_S3_ENDPOINT"), Bucket: "saba-test", AccessKeyID: os.Getenv("SABA_TEST_S3_ACCESS_KEY"), SecretAccessKey: "wrong"})
	if err := bad.Check(ctx); err == nil || !strings.Contains(err.Error(), "cannot write") {
		t.Fatalf("a wrong secret should fail the access check: %v", err)
	}
	missing, _ := New(Config{Endpoint: os.Getenv("SABA_TEST_S3_ENDPOINT"), Bucket: "saba-no-such-bucket", AccessKeyID: os.Getenv("SABA_TEST_S3_ACCESS_KEY"), SecretAccessKey: os.Getenv("SABA_TEST_S3_SECRET_KEY")})
	if err := missing.Check(ctx); err == nil || !strings.Contains(err.Error(), "NoSuchBucket") {
		t.Fatalf("a missing bucket should be named: %v", err)
	}
}

func TestConfig(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		cfg  Config
		host string
		path bool
	}{
		{Config{Bucket: "b", Region: "eu-west-1"}, "b.s3.eu-west-1.amazonaws.com", false},
		{Config{Bucket: "b"}, "b.s3.us-east-1.amazonaws.com", false},
		{Config{Bucket: "b", Endpoint: "https://acc.r2.cloudflarestorage.com/"}, "acc.r2.cloudflarestorage.com", true},
		{Config{Bucket: "b", Endpoint: "localhost:9000"}, "localhost:9000", true},
		{Config{Bucket: "b", Endpoint: "http://minio:9000", PathStyle: &no}, "b.minio:9000", false},
		{Config{Bucket: "b", Region: "us-west-2", PathStyle: &yes}, "s3.us-west-2.amazonaws.com", true},
	}
	for _, tc := range cases {
		tc.cfg.AccessKeyID, tc.cfg.SecretAccessKey = "k", "s"
		c, err := New(tc.cfg)
		if err != nil || c.base.Host != tc.host || c.pathStyle != tc.path {
			t.Errorf("%+v: host %q path-style %v (%v), want %q %v", tc.cfg, c.base.Host, c.pathStyle, err, tc.host, tc.path)
		}
	}
	for _, bad := range []Config{{}, {Bucket: "b"}, {Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s", Endpoint: "ftp://x"}} {
		if _, err := New(bad); err == nil {
			t.Errorf("%+v should be rejected", bad)
		}
	}
}

// The signature must match the worked example in the AWS documentation
// ("GET Object" in "Signature Calculations for the Authorization Header").
func TestSignatureMatchesAWSExample(t *testing.T) {
	c, _ := New(Config{Bucket: "examplebucket", Region: "us-east-1", AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"})
	c.now = func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }
	c.base.Host = "examplebucket.s3.amazonaws.com"
	req, err := c.request(context.Background(), "GET", "", map[string][]string{"lifecycle": {""}}, nil, 0, emptyHash)
	if err != nil {
		t.Fatal(err)
	}
	want := "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+want) {
		t.Fatalf("signature mismatch:\n got %s\nwant ...Signature=%s", got, want)
	}
}
