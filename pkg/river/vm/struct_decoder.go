package vm

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/grafana/agent/pkg/river/ast"
	"github.com/grafana/agent/pkg/river/diag"
	"github.com/grafana/agent/pkg/river/internal/reflectutil"
	"github.com/grafana/agent/pkg/river/internal/rivertags"
	"github.com/grafana/agent/pkg/river/internal/value"
)

// structDecoder decodes a series of AST statements into a Go value.
type structDecoder struct {
	VM      *Evaluator
	Scope   *Scope
	Assoc   map[value.Value]ast.Node
	TagInfo *tagInfo
}

// Decode decodes the list of statements into the struct value specified by rv.
func (st *structDecoder) Decode(stmts ast.Body, rv reflect.Value) error {
	// TODO(rfratto): potentially loosen this restriction and allow decoding into
	// an interface{} or map[string]interface{}.
	if rv.Kind() != reflect.Struct {
		panic(fmt.Sprintf("river/vm: structDecoder expects struct, got %s", rv.Kind()))
	}

	state := decodeOptions{
		Tags:       st.TagInfo.TagLookup,
		SeenAttrs:  make(map[string]struct{}),
		SeenBlocks: make(map[string]struct{}),

		BlockCount: make(map[string]int),
		BlockIndex: make(map[*ast.BlockStmt]int),
	}

	// Iterate over the set of blocks to populate block count and block index.
	// Block index is its index in the set of blocks with the *same name*.
	for _, stmt := range stmts {
		switch stmt := stmt.(type) {
		case *ast.BlockStmt:
			fullName := strings.Join(stmt.Name, ".")
			state.BlockIndex[stmt] = state.BlockCount[fullName]
			state.BlockCount[fullName]++
		}
	}

	for _, stmt := range stmts {
		switch stmt := stmt.(type) {
		case *ast.AttributeStmt:
			// TODO(rfratto): append to list of diagnostics instead of aborting early.
			if err := st.decodeAttr(stmt, rv, &state); err != nil {
				return err
			}

		case *ast.BlockStmt:
			// TODO(rfratto): append to list of diagnostics instead of aborting early.
			if err := st.decodeBlock(stmt, rv, &state); err != nil {
				return err
			}

		default:
			panic(fmt.Sprintf("river/vm: unrecognized node type %T", stmt))
		}
	}

	for _, tf := range st.TagInfo.Tags {
		// Ignore any optional tags.
		if tf.IsOptional() {
			continue
		}

		fullName := strings.Join(tf.Name, ".")

		switch {
		case tf.IsAttr():
			if _, consumed := state.SeenAttrs[fullName]; !consumed {
				// TODO(rfratto): change to diagnostics.
				return fmt.Errorf("missing required attribute %q", fullName)
			}

		case tf.IsBlock():
			if _, consumed := state.SeenBlocks[fullName]; !consumed {
				// TODO(rfratto): change to diagnostics.
				return fmt.Errorf("missing required block %q", fullName)
			}
		}
	}

	return nil
}

type decodeOptions struct {
	Tags       map[string]rivertags.Field
	SeenAttrs  map[string]struct{}
	SeenBlocks map[string]struct{}

	// BlockCount and BlockIndex are used to determine:
	//
	// * How big a slice of blocks should be for a block of a given name (BlockCount)
	// * Which element within that slice is a given block assigned to (BlockIndex)
	//
	// This is used for decoding a series of rule blocks for prometheus.relabel,
	// where 5 rules would have a "rule" key in BlockCount with a value of 5, and
	// where the first block would be index 0, the second block would be index 1,
	// and so on.
	//
	// The index in BlockIndex is relative to a block name; the first block named
	// "hello.world" and the first block named "fizz.buzz" both have index 0.

	BlockCount map[string]int         // Number of times a block by full name is seen.
	BlockIndex map[*ast.BlockStmt]int // Index of a block within a set of blocks with the same name.

}

func (st *structDecoder) decodeAttr(attr *ast.AttributeStmt, rv reflect.Value, state *decodeOptions) error {
	fullName := attr.Name.Name
	if _, seen := state.SeenAttrs[fullName]; seen {
		return diag.Diagnostics{{
			Severity: diag.SeverityLevelError,
			StartPos: ast.StartPos(attr).Position(),
			EndPos:   ast.EndPos(attr).Position(),
			Message:  fmt.Sprintf("attribute %q may only be provided once", fullName),
		}}
	}
	state.SeenAttrs[fullName] = struct{}{}

	tf, ok := state.Tags[fullName]
	if !ok {
		return diag.Diagnostics{{
			Severity: diag.SeverityLevelError,
			StartPos: ast.StartPos(attr).Position(),
			EndPos:   ast.EndPos(attr).Position(),
			Message:  fmt.Sprintf("unrecognized attribute name %q", fullName),
		}}
	} else if tf.IsBlock() {
		return diag.Diagnostics{{
			Severity: diag.SeverityLevelError,
			StartPos: ast.StartPos(attr).Position(),
			EndPos:   ast.EndPos(attr).Position(),
			Message:  fmt.Sprintf("%q must be a block, but is used as an attribute", fullName),
		}}
	}

	// Decode the attribute.
	val, err := st.VM.evaluateExpr(st.Scope, st.Assoc, attr.Value)
	if err != nil {
		// TODO(rfratto): get error as diagnostics.
		return err
	}

	// We're reconverting our reflect.Value back into an interface{}, so we
	// need to also turn it back into a pointer for decoding.
	field := reflectutil.FieldWalk(rv, tf.Index, true)
	if err := value.Decode(val, field.Addr().Interface()); err != nil {
		// TODO(rfratto): get error as diagnostics.
		return err
	}

	return nil
}

func (st *structDecoder) decodeBlock(block *ast.BlockStmt, rv reflect.Value, state *decodeOptions) error {
	fullName := strings.Join(block.Name, ".")
	tf, ok := state.Tags[fullName]
	if !ok {
		return diag.Diagnostics{{
			Severity: diag.SeverityLevelError,
			StartPos: ast.StartPos(block).Position(),
			EndPos:   ast.EndPos(block).Position(),
			Message:  fmt.Sprintf("unrecognized block name %q", fullName),
		}}
	} else if tf.IsAttr() {
		return diag.Diagnostics{{
			Severity: diag.SeverityLevelError,
			StartPos: ast.StartPos(block).Position(),
			EndPos:   ast.EndPos(block).Position(),
			Message:  fmt.Sprintf("%q must be an attribute, but is used as a block", fullName),
		}}
	}

	field := reflectutil.FieldWalk(rv, tf.Index, true)
	decodeField := prepareDecodeValue(field)

	switch decodeField.Kind() {
	case reflect.Slice:
		// If this is the first time we've seen the block, reset its length to
		// zero.
		if _, seen := state.SeenBlocks[fullName]; !seen {
			count := state.BlockCount[fullName]
			decodeField.Set(reflect.MakeSlice(decodeField.Type(), count, count))
		}

		blockIndex, ok := state.BlockIndex[block]
		if !ok {
			panic("river/vm: block not found in index lookup table")
		}
		decodeElement := prepareDecodeValue(decodeField.Index(blockIndex))
		err := st.VM.evaluateBlockOrBody(st.Scope, st.Assoc, block, decodeElement)
		if err != nil {
			// TODO(rfratto): get error as diagnostics.
			return err
		}

	case reflect.Array:
		if decodeField.Len() != state.BlockCount[fullName] {
			return diag.Diagnostics{{
				Severity: diag.SeverityLevelError,
				StartPos: ast.StartPos(block).Position(),
				EndPos:   ast.EndPos(block).Position(),
				Message: fmt.Sprintf(
					"block %q must be specified exactly %d times, but was specified %d times",
					fullName,
					decodeField.Len(),
					state.BlockCount[fullName],
				),
			}}
		}

		blockIndex, ok := state.BlockIndex[block]
		if !ok {
			panic("river/vm: block not found in index lookup table")
		}
		decodeElement := prepareDecodeValue(decodeField.Index(blockIndex))
		err := st.VM.evaluateBlockOrBody(st.Scope, st.Assoc, block, decodeElement)
		if err != nil {
			// TODO(rfratto): get error as diagnostics.
			return err
		}

	default:
		if state.BlockCount[fullName] > 1 {
			return diag.Diagnostics{{
				Severity: diag.SeverityLevelError,
				StartPos: ast.StartPos(block).Position(),
				EndPos:   ast.EndPos(block).Position(),
				Message:  fmt.Sprintf("block %q may only be specified once", fullName),
			}}
		}

		err := st.VM.evaluateBlockOrBody(st.Scope, st.Assoc, block, decodeField)
		if err != nil {
			// TODO(rfratto): get error as diagnostics.
			return err
		}
	}

	state.SeenBlocks[fullName] = struct{}{}
	return nil
}
