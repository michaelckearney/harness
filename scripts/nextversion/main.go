// Command nextversion prints the version the harness's next release would
// be, from the pull requests merged since its last tag, or nothing.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/michaelckearney/harness/internal/gitversion"
)

func main() {
	p, err := gitversion.Calculate(context.Background(), ".", gitversion.Series{})
	if err == nil {
		err = p.Strict()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if p.Releases() {
		fmt.Println(p.Next.Tag())
	}
}
