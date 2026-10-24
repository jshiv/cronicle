package cronicle

import (
	"strings"

	"github.com/hashicorp/hcl/v2/ext/tryfunc"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

// forEachFunctions are the functions a for_each expression can call: the
// pure ones of Terraform's built-in library, under Terraform's names and
// with its behavior, taken from go-cty's stdlib (which Terraform's own are
// built on). Functions that touch the filesystem, network, or clock are
// left out, as is anything Terraform implements outside that library
// (base64*, sha*, uuid, cidr*, index, …).
//
// They're available only in for_each: elsewhere each.* decode to
// placeholder tokens, which a function like upper() would corrupt.
var forEachFunctions = map[string]function.Function{
	// Collections
	"chunklist":       stdlib.ChunklistFunc,
	"coalesce":        stdlib.CoalesceFunc,
	"coalescelist":    stdlib.CoalesceListFunc,
	"compact":         stdlib.CompactFunc,
	"concat":          stdlib.ConcatFunc,
	"contains":        stdlib.ContainsFunc,
	"distinct":        stdlib.DistinctFunc,
	"element":         stdlib.ElementFunc,
	"flatten":         stdlib.FlattenFunc,
	"keys":            stdlib.KeysFunc,
	"length":          stdlib.LengthFunc,
	"lookup":          stdlib.LookupFunc,
	"merge":           stdlib.MergeFunc,
	"range":           stdlib.RangeFunc,
	"reverse":         stdlib.ReverseListFunc,
	"setintersection": stdlib.SetIntersectionFunc,
	"setproduct":      stdlib.SetProductFunc,
	"setsubtract":     stdlib.SetSubtractFunc,
	"setunion":        stdlib.SetUnionFunc,
	"slice":           stdlib.SliceFunc,
	"sort":            stdlib.SortFunc,
	"values":          stdlib.ValuesFunc,
	"zipmap":          stdlib.ZipmapFunc,

	// Type conversion
	"tobool":   stdlib.MakeToFunc(cty.Bool),
	"tolist":   stdlib.MakeToFunc(cty.List(cty.DynamicPseudoType)),
	"tomap":    stdlib.MakeToFunc(cty.Map(cty.DynamicPseudoType)),
	"tonumber": stdlib.MakeToFunc(cty.Number),
	"toset":    stdlib.MakeToFunc(cty.Set(cty.DynamicPseudoType)),
	"tostring": stdlib.MakeToFunc(cty.String),

	// Strings
	"chomp":      stdlib.ChompFunc,
	"format":     stdlib.FormatFunc,
	"formatlist": stdlib.FormatListFunc,
	"indent":     stdlib.IndentFunc,
	"join":       stdlib.JoinFunc,
	"lower":      stdlib.LowerFunc,
	"regex":      stdlib.RegexFunc,
	"regexall":   stdlib.RegexAllFunc,
	"replace":    replaceFunc,
	"split":      stdlib.SplitFunc,
	"strrev":     stdlib.ReverseFunc,
	"substr":     stdlib.SubstrFunc,
	"title":      stdlib.TitleFunc,
	"trim":       stdlib.TrimFunc,
	"trimprefix": stdlib.TrimPrefixFunc,
	"trimspace":  stdlib.TrimSpaceFunc,
	"trimsuffix": stdlib.TrimSuffixFunc,
	"upper":      stdlib.UpperFunc,

	// Numbers
	"abs":      stdlib.AbsoluteFunc,
	"ceil":     stdlib.CeilFunc,
	"floor":    stdlib.FloorFunc,
	"log":      stdlib.LogFunc,
	"max":      stdlib.MaxFunc,
	"min":      stdlib.MinFunc,
	"parseint": stdlib.ParseIntFunc,
	"pow":      stdlib.PowFunc,
	"signum":   stdlib.SignumFunc,

	// Encoding
	"csvdecode":  stdlib.CSVDecodeFunc,
	"jsondecode": stdlib.JSONDecodeFunc,
	"jsonencode": stdlib.JSONEncodeFunc,

	// Errors
	"can": tryfunc.CanFunc,
	"try": tryfunc.TryFunc,
}

// replaceFunc is Terraform's replace: a substring wrapped in slashes
// ("/[0-9]+/") is a regular expression, anything else is literal.
var replaceFunc = function.New(&function.Spec{
	Params: []function.Parameter{
		{Name: "str", Type: cty.String},
		{Name: "substr", Type: cty.String},
		{Name: "replace", Type: cty.String},
	},
	Type: function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
		sub := args[1].AsString()
		if len(sub) > 1 && strings.HasPrefix(sub, "/") && strings.HasSuffix(sub, "/") {
			return stdlib.RegexReplace(args[0], cty.StringVal(sub[1:len(sub)-1]), args[2])
		}
		return stdlib.Replace(args[0], args[1], args[2])
	},
})
