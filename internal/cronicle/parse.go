package cronicle

import (
	"encoding/json"
	"os"

	"regexp"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"log/slog"
)

// envContextVar builds the `env` namespace exposed to HCL eval —
// a cty object whose attributes are the names of env vars currently
// set, and whose values are their string values. `${env.FOO}` in any
// HCL string field substitutes to os.Getenv("FOO"). Names not in the
// process env are absent from the object; HCL will then return ""
// for those references rather than erroring, which lets `repo {
// password = "${env.X}" }` parse cleanly on a laptop where X isn't
// set (worker pods that DO have X take the populated value).
//
// The snapshot is taken at call time, not at package init, so
// RefreshEvalContext() picks up env changes made by long-running
// processes (e.g. a controller that learns its bearer token after
// startup) and by tests using t.Setenv.
func envContextVar() cty.Value {
	vars := map[string]cty.Value{}
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				vars[kv[:i]] = cty.StringVal(kv[i+1:])
				break
			}
		}
	}
	if len(vars) == 0 {
		return cty.EmptyObjectVal
	}
	return cty.ObjectVal(vars)
}

// RefreshEvalContext re-snapshots the `env` namespace on the package
// CommandEvalContext using the current process env. ParseFile calls
// this before decoding so a binary that mutates its env at runtime
// (e.g. a controller that picks up a token from a Secret mount) sees
// the fresh value. Tests that use t.Setenv + hclsimple.DecodeFile
// directly should call this between the two so the new value is
// visible during decode.
func RefreshEvalContext() {
	CommandEvalContext.Variables["env"] = envContextVar()
}

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
	//
	// The `env` namespace is populated lazily by envContextVars() — see
	// the var-initializer block below — so `${env.FOO}` in any HCL
	// string field resolves to os.Getenv("FOO") at parse time. Used by
	// repo { password = "${env.CRONICLE_TOKEN}" } to keep secrets out
	// of HCL files. Missing env vars resolve to "" rather than erroring
	// so HCL with unresolvable refs still parses (and the downstream
	// "empty password ⇒ anonymous auth" branch handles the runtime
	// case cleanly).
	CommandEvalContext = hcl.EvalContext{
		Variables: map[string]cty.Value{
			"date":      cty.StringVal("${date}"),
			"datetime":  cty.StringVal("${datetime}"),
			"timestamp": cty.StringVal("${timestamp}"),
			// scratch survives HCL parse as the literal "${scratch}"
			// token, then exec.go substitutes the schedule-scoped
			// scratch dir path at run time.
			"scratch": cty.StringVal("${scratch}"),
			// path is the task working directory (typically the
			// schedule's repo clone or the cronicle config dir).
			// Used in mcp { command = ["...", "${path}/data"] }
			// patterns where the server needs a real filesystem path.
			"path": cty.StringVal("${path}"),
			"env":  envContextVar(),
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
	destination, err := os.Create(path)
	if err != nil {
		panic(err)
	}

	_, writeErr := destination.Write(b)
	// _, writeErr := f.WriteTo(destination)
	if writeErr != nil {
		slog.Error("write error")
	}
	destination.Close()
	return path
}

//ParseFile parses a given hcl file into a Config
func ParseFile(cronicleFile string, parser *hclparse.Parser) (*Config, hcl.Diagnostics) {

	var diags hcl.Diagnostics

	file, parseDiags := parser.ParseHCLFile(cronicleFile)

	diags = append(diags, parseDiags...)
	if diags.HasErrors() {
		return nil, diags
	}

	return decodeConfigBody(file.Body)
}

// ParseBytes parses an in-memory HCL document into a Config. Used by
// the non-file ConfigSources (http, s3, postgres) which load bytes
// rather than file paths. The filename arg is used in parser diagnostics
// only — typically "cronicle.hcl" or the source's String() output.
func ParseBytes(src []byte, filename string, parser *hclparse.Parser) (*Config, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	file, parseDiags := parser.ParseHCL(src, filename)
	diags = append(diags, parseDiags...)
	if diags.HasErrors() {
		return nil, diags
	}
	return decodeConfigBody(file.Body)
}

// decodeConfigBody is the common gohcl-decode + AgentTasks-fold path
// shared by ParseFile and ParseBytes. Keeps the schema-evolution
// surface in one place.
func decodeConfigBody(body hcl.Body) (*Config, hcl.Diagnostics) {
	// Refresh env vars exposed via ${env.X} from the current process env
	// before each decode. Production binaries' env is typically static
	// at startup, but the controller pod re-reads its bearer from a
	// mounted Secret which can rotate, and tests using t.Setenv need
	// the new value visible during decode.
	RefreshEvalContext()
	var diags hcl.Diagnostics
	var conf Config
	decodeDiags := gohcl.DecodeBody(body, &CommandEvalContext, &conf)
	diags = append(diags, decodeDiags...)
	if diags.HasErrors() {
		return &conf, diags
	}

	// Fold each schedule's first-class `agent "X" { ... }` blocks into its
	// .Tasks slice. After this step downstream code (DAG, Validate, exec,
	// projection) sees a uniform list of Tasks regardless of whether the
	// operator wrote `agent "X"` or `task "X" { agent {} }`.
	for i := range conf.Schedules {
		s := &conf.Schedules[i]
		for _, at := range s.AgentTasks {
			s.Tasks = append(s.Tasks, at.ToTask())
		}
		s.AgentTasks = nil
	}
	return &conf, nil
}

// JSON method returns a json []byte array of the struct
func (conf Config) JSON() []byte {
	b, err := json.Marshal(&conf)
	if err != nil {
		slog.Error("json marshal failed", "error", err.Error())
	}
	return b
}

// JSON method returns a json []byte array of the struct
func (schedule Schedule) JSON() []byte {
	b, err := json.Marshal(&schedule)
	if err != nil {
		slog.Error("json marshal failed", "error", err.Error())
	}
	return b
}

// JSON method returns a json []byte array of the struct
func (task Task) JSON() []byte {
	b, err := json.Marshal(&task)
	if err != nil {
		slog.Error("json marshal failed", "error", err.Error())
	}
	return b
}

//Hcl returns a hcl File object from a given Config
func (conf Config) Hcl() HclWriteFile {
	f := hclwrite.NewEmptyFile()
	gohcl.EncodeIntoBody(&conf, f.Body())
	r := regexp.MustCompile("[$]+")
	b := r.ReplaceAllLiteral(f.Bytes(), []byte("$"))
	return HclWriteFile{File: *f, Bytes: b}
}

//Hcl returns a hcl File object from a given task
func (task Task) Hcl() HclWriteFile {
	f := hclwrite.NewEmptyFile()
	gohcl.EncodeIntoBody(&task, f.Body())
	r := regexp.MustCompile("[$]+")
	b := r.ReplaceAllLiteral(f.Bytes(), []byte("$"))
	return HclWriteFile{File: *f, Bytes: b}
}
