package orch

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

type blockKind int

const (
	blockSingleton blockKind = iota
	blockKeyed
)

var patchableBlocks = map[string]blockKind{
	"github":       blockSingleton,
	"orchestrator": blockSingleton,
	"vm":           blockKeyed,
	"target":       blockKeyed,
}

func patchHCL(src []byte, patch map[string]map[string]any) ([]byte, error) {
	f, diags := hclwrite.ParseConfig(src, "swarm.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse: %s", diags.Error())
	}
	for blockName, fields := range patch {
		parts := strings.SplitN(blockName, ".", 2)
		head := parts[0]
		kind, ok := patchableBlocks[head]
		if !ok {
			return nil, fmt.Errorf("block %q is not editable via the dashboard", blockName)
		}

		if head == "orchestrator" && len(parts) == 2 && parts[1] == "capture" {
			var orch *hclwrite.Block
			for _, b := range f.Body().Blocks() {
				if b.Type() == "orchestrator" {
					orch = b
					break
				}
			}
			if orch == nil {
				orch = f.Body().AppendNewBlock("orchestrator", nil)
			}
			var cap *hclwrite.Block
			for _, b := range orch.Body().Blocks() {
				if b.Type() == "capture" {
					cap = b
					break
				}
			}
			if cap == nil {
				cap = orch.Body().AppendNewBlock("capture", nil)
			}
			if err := writeAttrs(cap.Body(), fields, blockName); err != nil {
				return nil, err
			}
			continue
		}

		if kind == blockKeyed {
			if len(parts) != 2 {
				return nil, fmt.Errorf("%q requires a label (e.g. %q)", head, head+".name")
			}
			label := parts[1]
			var existing *hclwrite.Block
			for _, b := range f.Body().Blocks() {
				if b.Type() == head && len(b.Labels()) == 1 && b.Labels()[0] == label {
					existing = b
					break
				}
			}
			if _, del := fields["__delete"]; del {
				if existing != nil {
					f.Body().RemoveBlock(existing)
				}
				continue
			}
			if existing == nil {
				existing = f.Body().AppendNewBlock(head, []string{label})
			}
			if err := writeAttrs(existing.Body(), fields, blockName); err != nil {
				return nil, err
			}
			continue
		}

		// Singleton.
		var target *hclwrite.Block
		for _, b := range f.Body().Blocks() {
			if b.Type() == head {
				target = b
				break
			}
		}
		if target == nil {
			target = f.Body().AppendNewBlock(head, nil)
		}
		if err := writeAttrs(target.Body(), fields, blockName); err != nil {
			return nil, err
		}
	}
	return f.Bytes(), nil
}

func writeAttrs(body *hclwrite.Body, fields map[string]any, blockName string) error {
	for k, v := range fields {
		val, err := hclValueOf(v)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", blockName, k, err)
		}
		if val.IsNull() {
			body.RemoveAttribute(k)
		} else {
			body.SetAttributeValue(k, val)
		}
	}
	return nil
}

// hclValueOf converts a JSON-decoded value into a cty value suitable for
// hclwrite. Handles strings, bools, numbers, and homogeneous string lists
// — the only shapes the dashboard sends right now.
func hclValueOf(v any) (cty.Value, error) {
	switch x := v.(type) {
	case nil:
		return cty.NullVal(cty.DynamicPseudoType), nil
	case string:
		return cty.StringVal(x), nil
	case bool:
		return cty.BoolVal(x), nil
	case float64:
		if x == float64(int64(x)) {
			return cty.NumberIntVal(int64(x)), nil
		}
		return cty.NumberFloatVal(x), nil
	case []any:
		if len(x) == 0 {
			return cty.ListValEmpty(cty.String), nil
		}
		strs := make([]cty.Value, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return cty.NilVal, fmt.Errorf("list element not a string")
			}
			strs = append(strs, cty.StringVal(s))
		}
		return cty.ListVal(strs), nil
	}
	return cty.NilVal, fmt.Errorf("unsupported type %T", v)
}
