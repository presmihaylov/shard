// Command tag prints what services/kernel expects: the release tag, or with an arch, that file's hash.
package main

import (
	"fmt"
	"os"

	"github.com/presmihaylov/shard/services/kernel"
)

func main() {
	if len(os.Args) == 1 {
		fmt.Println(kernel.Tag())
		return
	}

	sum, err := kernel.SHA256(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(sum)
}
