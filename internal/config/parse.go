package config

import (
	"fmt"
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// CommandEvalContext hcl.EvalContext
var CommandEvalContext = hcl.EvalContext{
	Variables: map[string]cty.Value{
		"date": cty.StringVal("${date}"),
	},
}

//MarshallHcl writes a given Config to an hcl file at path
func MarshallHcl(conf Config, path string) string {
	f := hclwrite.NewEmptyFile()
	gohcl.EncodeIntoBody(&conf, f.Body())
	fmt.Printf("%s", f.Bytes())
	fmt.Println("writing to file")
	destination, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	_, writeErr := f.WriteTo(destination)
	if writeErr != nil {
		fmt.Printf("write error")
	}
	destination.Close()
	return path
}

// GetHcl returns a hcl File object from a given Config
func GetHcl(conf Config) *hclwrite.File {
	f := hclwrite.NewEmptyFile()
	gohcl.EncodeIntoBody(&conf, f.Body())
	return f
}
