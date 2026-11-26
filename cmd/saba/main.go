// Command saba deploys and runs apps on your own servers.
package main

import (
	"os"

	"github.com/egoist/saba/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
