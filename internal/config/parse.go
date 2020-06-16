package config

import (
	"fmt"
	"os"

	"regexp"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// HclWriteFile contains the encoded hclwrite.File and the byte array of the file with
// the $$ deduped. This is due the the effect that when writing template arguments with
// the hcl library an extra $ will be added automatically. i.e. "${date}" becomes "$${date}"
type HclWriteFile struct {
	// File is the hclwrite.File encoded by gohcl.EncodeIntoBody
	File hclwrite.File

	// Bytes is the byte array with deduped $$
	Bytes []byte
}

var (
	// CommandEvalContext hcl.EvalContext evaluates the "${date}" argument and carries it through
	// as a string of the same form that will be used as an arugment later in the code.
	CommandEvalContext = hcl.EvalContext{
		Variables: map[string]cty.Value{
			"date":      cty.StringVal("${date}"),
			"datetime":  cty.StringVal("${datetime}"),
			"timestamp": cty.StringVal("${timestamp}"),
		},
	}

	// TimeArgumentFormatMap maps the CommandEvalContext arguments to time.Format strings for reforamting
	// arguments given in hcl to timestamps.
	// ${date}: 		"2006-01-02"
	// ${datetime}: 	"2006-01-02T15:04:05Z07:00"
	// ${timestamp}: 	"2006-01-02 15:04:05Z07:00"
	TimeArgumentFormatMap = map[string]string{
		"${date}":      "2006-01-02",
		"${datetime}":  time.RFC3339,
		"${timestamp}": "2006-01-02 15:04:05Z07:00",
	}
)

//MarshallHcl writes a given Config to an hcl file at path
func MarshallHcl(conf Config, path string) string {
	f := hclwrite.NewEmptyFile()
	gohcl.EncodeIntoBody(&conf, f.Body())
	r := regexp.MustCompile("[$]+")
	b := r.ReplaceAllLiteral(f.Bytes(), []byte("$"))
	fmt.Printf("%s", b)
	fmt.Println("writing to file")
	destination, err := os.Create(path)
	if err != nil {
		panic(err)
	}

	_, writeErr := destination.Write(b)
	// _, writeErr := f.WriteTo(destination)
	if writeErr != nil {
		fmt.Printf("write error")
	}
	destination.Close()
	return path
}

// GetHcl returns a hcl File object from a given Config
func GetHcl(conf Config) HclWriteFile {
	f := hclwrite.NewEmptyFile()
	gohcl.EncodeIntoBody(&conf, f.Body())
	r := regexp.MustCompile("[$]+")
	b := r.ReplaceAllLiteral(f.Bytes(), []byte("$"))
	return HclWriteFile{File: *f, Bytes: b}
}
