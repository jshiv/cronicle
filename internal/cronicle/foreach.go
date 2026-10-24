package cronicle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// A task with `for_each` runs once per item of a collection, Terraform
// style. The collection is usually the JSON stdout of an upstream task,
// so it's only known once that task succeeds:
//
//	task "list_prs" {
//	  command = ["gh", "pr", "list", "--json", "number,headRefOid"]
//	}
//
//	task "review" {
//	  for_each = { for pr in task.list_prs.output : pr.number => pr }
//	  command  = ["./review", each.key, each.value.headRefOid]
//	}
//
// When list_prs succeeds, review expands into review[12], review[15], …,
// each a full task in the DAG with its own logs, status, and rerun line.
// Two for_each tasks over the same source that depend on each other pair
// up item by item (report[12] waits only on review[12]); a plain task that
// depends on a for_each task waits for every item (fan-in).

// maxForEachItems caps one expansion. A collection bigger than this is far
// more likely a mistake (the wrong field, a runaway listing) than intent.
const maxForEachItems = 1000

// ForEach is a task's parsed `for_each`. It travels in the schedule's JSON
// payload, so it holds the expression's source text rather than an
// hcl.Expression, and is re-parsed where the run executes.
type ForEach struct {
	// Expr is the HCL source of the for_each expression.
	Expr string
	// Source is the upstream task whose output Expr reads (via
	// task.<name>.output), or "" for a static collection.
	Source string
	// MaxParallel caps how many of this task's items run at once; 0 is
	// no limit beyond the DAG's own.
	MaxParallel int `json:",omitempty"`
	// Items is the expanded collection recorded by an earlier run. Set
	// when that run is retried, so the retry works on the original items
	// instead of discovering a different set; Source is not consulted.
	Items []ForEachItem `json:",omitempty"`
	// Skip lists keys of Items that already succeeded and aren't re-run.
	Skip []string `json:",omitempty"`
}

// ForEachItem is one element of an expanded collection.
type ForEachItem struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// instanceName is how an item's task is named everywhere: in the DAG,
// logs, run state, and file names (see fileSafeName).
func instanceName(task, key string) string {
	return task + "[" + key + "]"
}

// splitInstanceName undoes instanceName: "review[12]" → ("review", "12").
func splitInstanceName(name string) (task, key string, ok bool) {
	i := strings.IndexByte(name, '[')
	if i <= 0 || !strings.HasSuffix(name, "]") {
		return "", "", false
	}
	return name[:i], name[i+1 : len(name)-1], true
}

// parseForEach reads a task's for_each attribute. src is the whole file,
// so the expression's text can be kept. Returns nil when the attribute is
// absent (gohcl hands back a null static expression for that).
func parseForEach(expr hcl.Expression, src []byte, maxParallel int) (*ForEach, hcl.Diagnostics) {
	if expr == nil {
		return nil, nil
	}
	if len(expr.Variables()) == 0 {
		if v, diags := expr.Value(nil); !diags.HasErrors() && v.IsNull() {
			return nil, nil
		}
	}
	rng := expr.Range()
	if rng.Start.Byte < 0 || rng.End.Byte > len(src) || rng.Start.Byte >= rng.End.Byte {
		return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid for_each", Detail: "for_each expression has no source text.", Subject: &rng}}
	}
	fe := &ForEach{Expr: string(src[rng.Start.Byte:rng.End.Byte]), MaxParallel: maxParallel}

	for _, tr := range expr.Variables() {
		root := tr.RootName()
		if root != "task" {
			if root == "each" {
				return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid for_each", Detail: "each.key and each.value are only available inside the task, not in for_each itself.", Subject: tr.SourceRange().Ptr()}}
			}
			continue // env.X and friends resolve at eval time
		}
		name, ok := traversalAttr(tr, 1)
		if !ok || len(tr) < 3 {
			return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid for_each", Detail: `Refer to an upstream task's output as task.<name>.output, e.g. for_each = task.list_prs.output`, Subject: tr.SourceRange().Ptr()}}
		}
		if attr, _ := traversalAttr(tr, 2); attr != "output" {
			return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid for_each", Detail: fmt.Sprintf("task.%s has no attribute %q; use task.%s.output (the task's stdout, parsed as JSON).", name, attr, name), Subject: tr.SourceRange().Ptr()}}
		}
		if fe.Source != "" && fe.Source != name {
			return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid for_each", Detail: fmt.Sprintf("for_each reads both task.%s and task.%s; a collection can come from one task.", fe.Source, name), Subject: tr.SourceRange().Ptr()}}
		}
		fe.Source = name
	}
	if fe.Source == "" {
		// Static collection: catch a bad literal now, not on the first run.
		if _, err := fe.evaluate(nil); err != nil {
			return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid for_each", Detail: err.Error(), Subject: &rng}}
		}
	}
	return fe, nil
}

// traversalAttr returns the attribute name at step i of a traversal.
func traversalAttr(tr hcl.Traversal, i int) (string, bool) {
	if i >= len(tr) {
		return "", false
	}
	switch step := tr[i].(type) {
	case hcl.TraverseRoot:
		return step.Name, true
	case hcl.TraverseAttr:
		return step.Name, true
	}
	return "", false
}

// expand turns the collection into items: from the recorded Items when a
// run is retried, otherwise by evaluating Expr against the source task's
// stdout.
func (fe *ForEach) expand(outputs map[string]string) ([]ForEachItem, error) {
	if fe.Items != nil {
		skip := make(map[string]bool, len(fe.Skip))
		for _, k := range fe.Skip {
			skip[k] = true
		}
		items := make([]ForEachItem, 0, len(fe.Items))
		for _, it := range fe.Items {
			if !skip[it.Key] {
				items = append(items, it)
			}
		}
		return items, nil
	}
	return fe.evaluate(outputs)
}

// evaluate parses and evaluates Expr. outputs holds each finished task's
// stdout; only Source's is used.
func (fe *ForEach) evaluate(outputs map[string]string) ([]ForEachItem, error) {
	expr, diags := hclsyntax.ParseExpression([]byte(fe.Expr), "for_each", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("for_each: %s", diags.Error())
	}
	vars := map[string]cty.Value{"env": envContextVar()}
	if fe.Source != "" {
		raw, ok := outputs[fe.Source]
		if !ok {
			return nil, fmt.Errorf("for_each reads task.%s.output, but %s did not run in this run (run the whole schedule so it runs first)", fe.Source, fe.Source)
		}
		out, err := jsonToCty(raw)
		if err != nil {
			return nil, fmt.Errorf("for_each: output of %s is not JSON: %v", fe.Source, err)
		}
		vars["task"] = cty.ObjectVal(map[string]cty.Value{
			fe.Source: cty.ObjectVal(map[string]cty.Value{"output": out}),
		})
	}
	v, diags := expr.Value(&hcl.EvalContext{Variables: vars, Functions: forEachFunctions})
	if diags.HasErrors() {
		return nil, fmt.Errorf("for_each: %s", diags.Error())
	}
	return collectionItems(v)
}

func jsonToCty(raw string) (cty.Value, error) {
	b := bytes.TrimSpace([]byte(raw))
	if len(b) == 0 {
		return cty.NilVal, fmt.Errorf("the task printed nothing")
	}
	ty, err := ctyjson.ImpliedType(b)
	if err != nil {
		return cty.NilVal, err
	}
	return ctyjson.Unmarshal(b, ty)
}

// collectionItems applies Terraform's for_each rules exactly: the value
// must be a map (or object), one item per key, or a set of strings, one
// item per element with each.key == each.value. Both iterate in sorted
// order. A list is not a set — it has positions and allows duplicates —
// so, as in Terraform, it must be converted explicitly with toset(), or
// turned into a map with a for expression.
func collectionItems(v cty.Value) ([]ForEachItem, error) {
	if v.IsNull() {
		return nil, fmt.Errorf("for_each value is null; it must be a map, or set of strings")
	}
	if !v.IsWhollyKnown() {
		return nil, fmt.Errorf("for_each value is not known")
	}
	ty := v.Type()
	var items []ForEachItem
	switch {
	case ty.IsMapType() || ty.IsObjectType():
		for it := v.ElementIterator(); it.Next(); {
			k, val := it.Element()
			raw, err := ctyjson.Marshal(val, val.Type())
			if err != nil {
				return nil, fmt.Errorf("for_each item %q: %v", k.AsString(), err)
			}
			items = append(items, ForEachItem{Key: k.AsString(), Value: raw})
		}
	case ty.IsSetType():
		if et := ty.ElementType(); et != cty.String && et != cty.DynamicPseudoType {
			return nil, fmt.Errorf("for_each supports maps and sets of strings, but you have provided a set containing type %s", et.FriendlyName())
		}
		for it := v.ElementIterator(); it.Next(); {
			_, val := it.Element()
			if val.IsNull() {
				return nil, fmt.Errorf("for_each set includes a null value")
			}
			raw, _ := json.Marshal(val.AsString())
			items = append(items, ForEachItem{Key: val.AsString(), Value: raw})
		}
	default:
		hint := ""
		if ty.IsListType() || ty.IsTupleType() {
			hint = "; convert a list of strings with toset(...), or build a map with a for expression, e.g. { for x in <list> : x.id => x }"
		}
		return nil, fmt.Errorf("for_each must be a map, or set of strings, and you have provided a value of type %s%s", ty.FriendlyName(), hint)
	}
	if len(items) > maxForEachItems {
		return nil, fmt.Errorf("for_each produced %d items, over the limit of %d", len(items), maxForEachItems)
	}
	for _, it := range items {
		if it.Key == "" || strings.ContainsAny(it.Key, "[]\n\r") {
			return nil, fmt.Errorf("for_each key %q can't be used as a task name (empty, or has brackets or a newline)", it.Key)
		}
	}
	return items, nil
}

// ---- each.key / each.value ------------------------------------------------

// HCL needs every variable bound when the config is decoded, but each
// item's values only exist at run time. So decoding binds `each` to an
// object whose leaves are placeholder tokens — each.value.sha decodes to
// the literal "${each.value.sha}" — the same trick ${date} uses, and
// instance() fills them in once the item is known.

// eachVariable builds the `each` object for decoding from every each.*
// reference in the file.
func eachVariable(body hcl.Body) (cty.Value, hcl.Diagnostics) {
	syn, ok := body.(*hclsyntax.Body)
	if !ok {
		return cty.EmptyObjectVal, nil
	}
	type node struct {
		leaf     bool
		children map[string]*node
	}
	root := &node{children: map[string]*node{}}
	var diags hcl.Diagnostics
	hclsyntax.VisitAll(syn, func(n hclsyntax.Node) hcl.Diagnostics {
		st, ok := n.(*hclsyntax.ScopeTraversalExpr)
		if !ok || st.Traversal.RootName() != "each" {
			return nil
		}
		path, err := eachPath(st.Traversal)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Invalid each reference", Detail: err.Error(), Subject: st.SrcRange.Ptr()})
			return nil
		}
		cur := root
		for _, seg := range path[1:] {
			next, ok := cur.children[seg]
			if !ok {
				next = &node{children: map[string]*node{}}
				cur.children[seg] = next
			}
			cur = next
		}
		cur.leaf = true
		return nil
	})
	if diags.HasErrors() {
		return cty.NilVal, diags
	}
	var build func(n *node, path []string) (cty.Value, error)
	build = func(n *node, path []string) (cty.Value, error) {
		if len(n.children) == 0 {
			return cty.StringVal(eachToken(path)), nil
		}
		if n.leaf {
			p := strings.Join(path, ".")
			return cty.NilVal, fmt.Errorf("%s is used both whole and by field; use only its fields, or read the whole item from $EACH_VALUE", p)
		}
		attrs := make(map[string]cty.Value, len(n.children))
		for seg, c := range n.children {
			v, err := build(c, append(append([]string(nil), path...), seg))
			if err != nil {
				return cty.NilVal, err
			}
			attrs[seg] = v
		}
		return cty.ObjectVal(attrs), nil
	}
	v, err := build(root, []string{"each"})
	if err != nil {
		return cty.NilVal, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid each reference", Detail: err.Error()}}
	}
	return v, nil
}

// eachPath turns each.value.sha / each.value["sha"] / each.value[0] into
// ["each", "value", "sha"] (or ["each", "value", "0"]).
func eachPath(tr hcl.Traversal) ([]string, error) {
	path := []string{"each"}
	for _, step := range tr[1:] {
		var seg string
		switch s := step.(type) {
		case hcl.TraverseAttr:
			seg = s.Name
		case hcl.TraverseIndex:
			k, err := convert.Convert(s.Key, cty.String)
			if err != nil || k.IsNull() || !k.IsKnown() {
				return nil, fmt.Errorf("each index must be a string or number")
			}
			seg = k.AsString()
		default:
			return nil, fmt.Errorf("unsupported each reference")
		}
		if seg == "" || strings.ContainsAny(seg, ".}") {
			return nil, fmt.Errorf("each field %q can't contain '.' or '}'", seg)
		}
		path = append(path, seg)
	}
	if len(path) < 2 || (path[1] != "key" && path[1] != "value") {
		return nil, fmt.Errorf("each has only each.key and each.value")
	}
	if path[1] == "key" && len(path) > 2 {
		return nil, fmt.Errorf("each.key is a string and has no fields")
	}
	return path, nil
}

func eachToken(path []string) string {
	return "${" + strings.Join(path, ".") + "}"
}

var eachTokenRE = regexp.MustCompile(`\$\{each\.(key|value)((?:\.[^.}]+)*)\}`)

// usesEach reports whether s holds an each.* placeholder.
func usesEach(s string) bool { return eachTokenRE.MatchString(s) }

// substituteEach fills each.* placeholders in s from one item. Values are
// passed as plain text — a string as itself, anything else as compact
// JSON — and never evaluated, so an item can't inject HCL. (A value put
// inside an `sh -c` script is still shell code to that shell: scripts
// should read $EACH_KEY / $EACH_VALUE from the environment instead.)
func substituteEach(s string, item ForEachItem) (string, error) {
	var firstErr error
	out := eachTokenRE.ReplaceAllStringFunc(s, func(tok string) string {
		m := eachTokenRE.FindStringSubmatch(tok)
		if m[1] == "key" {
			return item.Key
		}
		var fields []string
		if m[2] != "" {
			fields = strings.Split(m[2][1:], ".")
		}
		v, err := jsonField(item.Value, fields)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %v", strings.Trim(tok, "${}"), err)
		}
		return v
	})
	return out, firstErr
}

// jsonField walks fields into a JSON value and renders the result as text.
func jsonField(raw json.RawMessage, fields []string) (string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return "", err
	}
	for _, f := range fields {
		switch c := v.(type) {
		case map[string]any:
			next, ok := c[f]
			if !ok {
				return "", fmt.Errorf("no field %q", f)
			}
			v = next
		case []any:
			var i int
			if _, err := fmt.Sscan(f, &i); err != nil || i < 0 || i >= len(c) {
				return "", fmt.Errorf("no index %s", f)
			}
			v = c[i]
		default:
			return "", fmt.Errorf("no field %q (value is not an object)", f)
		}
	}
	switch x := v.(type) {
	case string:
		return x, nil
	case nil:
		return "", nil
	case json.Number:
		return x.String(), nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// instance builds the task for one item, named name[key]. Its each.*
// placeholders stay in place until it runs (fillEach), so an item's text
// is inserted after ${date}-style tokens and $secret.NAME references are
// resolved, and can't be mistaken for either. The returned error is the
// first placeholder this item can't fill (a missing field), which fails
// that item alone.
func (task Task) instance(item ForEachItem) (Task, error) {
	inst := task
	inst.Name = instanceName(task.Name, item.Key)
	inst.ForEach = nil
	inst.forEachOf = task.Name
	inst.eachItem = &item

	fields := append(append([]string(nil), task.Command...), task.Env...)
	if task.Agent != nil {
		fields = append(fields, task.Agent.Prompt, task.Agent.System)
		for _, m := range task.Agent.MCPs {
			fields = append(fields, m.Command...)
		}
	}
	var firstErr error
	for _, f := range fields {
		if _, err := substituteEach(f, item); err != nil {
			firstErr = err
			break
		}
	}
	// A task with its own repo checkout gets one per item, so parallel
	// items never share (and fight over) a working tree.
	if task.Repo != nil && task.Path != "" && task.Path != task.CroniclePath {
		inst.Path = filepath.Join(filepath.Dir(task.Path), fileSafeName(inst.Name))
	}
	return inst, firstErr
}

// fillEach fills each.* placeholders in s for an item; other tasks get s
// back unchanged. Call it last, on text already otherwise resolved.
func (task *Task) fillEach(s string) string {
	if task.eachItem == nil {
		return s
	}
	out, _ := substituteEach(s, *task.eachItem) // checked when the item was built
	return out
}

// eachEnv fills each.* in an item's resolved env and adds EACH_KEY and
// EACH_VALUE.
func (task *Task) eachEnv(env []string) []string {
	if task.eachItem == nil {
		return env
	}
	out := make([]string, 0, len(env)+2)
	for _, e := range env {
		out = append(out, task.fillEach(e))
	}
	return append(out, "EACH_KEY="+task.eachItem.Key, "EACH_VALUE="+eachValueText(task.eachItem.Value))
}

// rerunLine is the pasteable command for a failed shell task. An item's
// line sets EACH_KEY and EACH_VALUE first, so a script reading them
// reruns the same way.
func (task *Task) rerunLine(cmd []string) string {
	line := shellQuote(cmd)
	if task.eachItem == nil {
		return line
	}
	return "EACH_KEY=" + shellQuoteArg(task.eachItem.Key) +
		" EACH_VALUE=" + shellQuoteArg(eachValueText(task.eachItem.Value)) + " " + line
}

// eachValueText is EACH_VALUE: a string item as itself, anything else as
// JSON.
func eachValueText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._\[\]=+,@-]`)

// fileSafeName makes a task name usable as one path element: an item key
// like "org/repo" or "../x" must not escape the directory it's written to.
func fileSafeName(name string) string {
	s := unsafeFileChars.ReplaceAllString(name, "_")
	if s == "." || s == ".." {
		return "_"
	}
	return s
}

// forEachKeys lists an expansion's keys, for logs.
func forEachKeys(items []ForEachItem) []string {
	keys := make([]string, len(items))
	for i, it := range items {
		keys[i] = it.Key
	}
	return keys
}
