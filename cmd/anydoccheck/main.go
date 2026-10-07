package main

import (
"fmt"

anydoc "github.com/firecrawl/anydoc/gogo"
)

func main() {
f := anydoc.FormatXlsx
md, err := anydoc.ToMarkdownBytes([]byte("dummy"), &f)
fmt.Println("md:", len(md), "err:", err)
}
