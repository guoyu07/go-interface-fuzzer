// Parse fuzzer special comments.
//
// For all the WantedFuzzerFrom* functions, if the parameter does not
// contain a fuzzer special comment, the default wanted fuzzer is
// returned and the error is non-nil.
//
// See the README for a comprehensive explanation of the special
// comments.

package main

import (
	"errors"
	"fmt"
	"go/ast"
	"strings"
	"unicode"
)

// WantedFuzzer is a description of a fuzzer we want to generate.
type WantedFuzzer struct {
	// The name of the interface.
	InterfaceName string

	// The function to produce a reference implementation.
	Reference Function

	// If true, the reference function returns a value rather than a
	// pointer.
	ReturnsValue bool

	// Invariant expressions.
	Invariants []string

	// Comparison functions to use. The keys of this map are
	// ToString'd Types.
	Comparison map[string]EitherFunctionOrMethod

	// Generator functions The keys of this map are ToString'd Types.
	Generator map[string]Generator

	// Initial state for custom generator functions.
	GeneratorState string
}

// Generator is the name of a function to generate a value of a given
// type.
type Generator struct {
	// True if this is stateful.
	IsStateful bool

	// The function itself.
	Name string
}

// EitherFunctionOrMethod is either a function or a method. Param and
// receiver types are all the same.
type EitherFunctionOrMethod struct {
	// True if this is function, rather than a method.
	IsFunction bool

	// The function itself.
	Name string

	// The type of the method receiver / function parameters.
	Type Type

	// List of return types. Only meaningful in "@before compare".
	Returns []Type
}

// WantedFuzzersFromAST extracts all wanted fuzzers from comments in
// the AST of a file.
func WantedFuzzersFromAST(theAST *ast.File) (wanteds []WantedFuzzer, errs []error) {
	if theAST == nil {
		return nil, nil
	}

	if theAST.Doc != nil {
		wanted, err := WantedFuzzersFromCommentGroup(theAST.Doc)
		if err == nil {
			wanteds = append(wanteds, wanted...)
		} else {
			errs = append(errs, err)
		}
	}

	for _, group := range theAST.Comments {
		wanted, err := WantedFuzzersFromCommentGroup(group)
		if err == nil {
			wanteds = append(wanteds, wanted...)
		} else {
			errs = append(errs, err)
		}
	}

	return wanteds, errs
}

// WantedFuzzersFromCommentGroup extracts all wanted fuzzer
// descriptions from a comment group. The "@fuzz interface:" line
// starts a new fuzzer definition; special comments in a group before
// this are ignored.
func WantedFuzzersFromCommentGroup(group *ast.CommentGroup) ([]WantedFuzzer, error) {
	if group == nil {
		return nil, nil
	}

	var commentLines []string
	for _, comment := range group.List {
		lines := splitLines(comment.Text)
		commentLines = append(commentLines, lines...)
	}

	return WantedFuzzersFromCommentLines(commentLines)
}

// WantedFuzzersFromCommentLines extracts all wanted fuzzer
// descriptions from a collection of comment lines. The "@fuzz
// interface:" line starts a new fuzzer definition; special comments
// in a group before this are ignored.
func WantedFuzzersFromCommentLines(commentLines []string) ([]WantedFuzzer, error) {
	if commentLines == nil {
		return nil, nil
	}

	var fuzzers []WantedFuzzer
	var fuzzer WantedFuzzer

	// 'fuzzing' indicates whether we've found the start of a
	// special comment or not. If not, just look for "@fuzz
	// interface" and ignore everything else.
	fuzzing := false

	for _, line := range commentLines {
		line = strings.TrimSpace(line)

		var err error
		if fuzzing {
			err = parseLine(line, &fuzzer)
		} else {
			// "@fuzz interface:"
			suff, ok := matchPrefix(line, "@fuzz interface:")
			if !ok {
				continue
			}

			if fuzzing {
				// Found a new fuzzer! Add the old one to the list.
				if fuzzer.Reference.Name == "" {
					return fuzzers, fmt.Errorf("fuzzer declaration for %s missing '@known correct' line", fuzzer.InterfaceName)
				}
				fuzzers = append(fuzzers, fuzzer)

			}

			var name string
			name, err = parseFuzzInterface(suff)
			fuzzer = WantedFuzzer{
				InterfaceName: name,
				Comparison:    make(map[string]EitherFunctionOrMethod),
				Generator:     make(map[string]Generator),
			}
			fuzzing = true
		}

		if err != nil {
			return fuzzers, err
		}

	}

	if fuzzing {
		// Add the final fuzzer to the list.
		if fuzzer.Reference.Name == "" {
			return fuzzers, fmt.Errorf("fuzzer declaration for %s missing '@known correct' line", fuzzer.InterfaceName)
		}
		return append(fuzzers, fuzzer), nil
	}

	return fuzzers, nil
}

/* Parse a line in a comment. If this is a special comment, handle it
and mutate the wanted fuzzer; if not, skip over.

SYNTAX: @known correct:   <parseKnownCorrect>
      | @invariant:       <parseInvariant>
      | @comparison:      <parseComparison>
      | @generator:       <parseGenerator>
      | @generator state: <parseGeneratorState>
*/
func parseLine(line string, fuzzer *WantedFuzzer) error {
	// "@known correct:"
	suff, ok := matchPrefix(line, "@known correct:")
	if ok {
		fundecl, returnsValue, err := parseKnownCorrect(suff)
		if err != nil {
			return err
		}

		retty := BasicType(fuzzer.InterfaceName)
		fundecl.Returns = []Type{&retty}
		fuzzer.Reference = fundecl
		fuzzer.ReturnsValue = returnsValue
	}

	// "@invariant:"
	suff, ok = matchPrefix(line, "@invariant:")
	if ok {
		inv, err := parseInvariant(suff)
		if err != nil {
			return err
		}

		fuzzer.Invariants = append(fuzzer.Invariants, inv)
	}

	// "@comparison:"
	suff, ok = matchPrefix(line, "@comparison:")
	if ok {
		tyname, fundecl, err := parseComparison(suff)
		if err != nil {
			return err
		}

		fuzzer.Comparison[tyname.ToString()] = fundecl
	}

	// "@generator:"
	suff, ok = matchPrefix(line, "@generator:")
	if ok {
		tyname, genfunc, stateful, err := parseGenerator(suff)
		if err != nil {
			return err
		}

		fuzzer.Generator[tyname.ToString()] = Generator{IsStateful: stateful, Name: genfunc}
	}

	// "@generator state:"
	suff, ok = matchPrefix(line, "@generator state:")
	if ok {
		state, err := parseGeneratorState(suff)
		if err != nil {
			return err
		}

		fuzzer.GeneratorState = state
	}

	return nil
}

// Parse a "@fuzz interface:"
//
// SYNTAX: Name
func parseFuzzInterface(line string) (string, error) {
	var (
		name string
		err  error
		rest string
	)

	name, rest = parseName(line)

	if name == "" {
		err = fmt.Errorf("expected a name in '%s'", line)
	} else if rest != "" {
		err = fmt.Errorf("unexpected left over input in '%s' (got '%s')", line, rest)
	}

	return name, err
}

// Parse a "@known correct:"
//
// SYNTAX: [&] FunctionName [ArgType1 ... ArgTypeN]
func parseKnownCorrect(line string) (Function, bool, error) {
	var function Function

	if len(line) == 0 {
		return function, false, errors.New("@known correct has empty argument")
	}

	// [&]
	rest, returnsValue := matchPrefix(line, "&")

	// FunctionName
	if len(line) == 0 {
		return function, false, errors.New("@known correct must have a function name")
	}

	function.Name, rest = parseFunctionName(rest)

	// [ArgType1 ... ArgTypeN]
	var args []Type
	for rest != "" {
		var argty Type
		var err error
		argty, rest, err = parseType(rest)

		if err != nil {
			return function, false, err
		}

		args = append(args, argty)
	}
	function.Parameters = args

	return function, returnsValue, nil
}

// Parse a "@comparison:"
//
// SYNTAX: (Type:FunctionName | FunctionName Type)
func parseComparison(line string) (Type, EitherFunctionOrMethod, error) {
	funcOrMeth, rest, err := parseFunctionOrMethod(line)

	if err != nil {
		return nil, funcOrMeth, err
	}
	if rest != "" {
		return nil, funcOrMeth, fmt.Errorf("unexpected left over input in '%s' (got '%s')", line, rest)
	}

	return funcOrMeth.Type, funcOrMeth, err
}

// Parse a "@generator:"
//
// SYNTAX: [!] FunctionName Type
func parseGenerator(line string) (Type, string, bool, error) {
	// [!]
	rest, stateful := matchPrefix(line, "!")

	// FunctionName
	var name string
	name, rest = parseFunctionName(rest)

	if name == "" {
		return nil, name, stateful, fmt.Errorf("expected a name in '%s'", line)
	}

	var err error
	var ty Type
	ty, rest, err = parseType(rest)

	if rest != "" {
		err = fmt.Errorf("unexpected left over input in '%s' (got '%s')", line, rest)
	}

	return ty, name, stateful, err
}

// Parse a "@generator state:"
//
// This does absolutely NO checking whatsoever beyond presence
// checking!
//
// SYNTAX: Expression
func parseGeneratorState(line string) (string, error) {
	if line == "" {
		return "", fmt.Errorf("expected an initial state")
	}

	return line, nil
}

// Parse an "@invariant:"
//
// This does absolutely NO checking whatsoever beyond presence
// checking!
//
// SYNTAX: Expression
func parseInvariant(line string) (string, error) {
	if line == "" {
		return "", fmt.Errorf("expected an expression")
	}

	return line, nil
}

// Parse a function or a method, returning the remainder of the
// string, which has leading spaces stripped.
//
// SYNTAX: (Type:FunctionName | FunctionName Type)
func parseFunctionOrMethod(line string) (EitherFunctionOrMethod, string, error) {
	var (
		funcOrMeth EitherFunctionOrMethod
		rest       string
		err        error
	)

	// This is a bit tricky, as there is overlap between names and
	// types. Try parsing as both a name and a type: if the type
	// succeeds, assume it's a method and go with that; if not and the
	// name succeeds assume it's a function; and if neither succeed
	// give an error.

	tyType, tyRest, tyErr := parseType(line)
	nName, nRest := parseFunctionName(line)

	if tyErr == nil && tyRest[0] == ':' {
		// It's a method.
		funcOrMeth.Type = tyType
		funcOrMeth.Name, rest = parseFunctionName(tyRest[1:])
	} else if nName != "" {
		// It's a function
		funcOrMeth.Name = nName
		funcOrMeth.Type, rest, err = parseType(nRest)
		funcOrMeth.IsFunction = true
	} else {
		err = fmt.Errorf("'%s' does not appear to be a method or function", line)
	}

	return funcOrMeth, rest, err
}

// Parse a function name, returning the remainder of the string, which
// has leading spaces stripped.
//
// SYNTAX: [ModuleName.].FunctionName
func parseFunctionName(line string) (string, string) {
	var (
		name string
		rest string
	)

	// Parse a name and see if the next character is a '.'.
	pref, suff := parseName(line)
	suff = strings.TrimLeftFunc(suff, unicode.IsSpace)

	if len(suff) > 0 && suff[0] == '.' {
		modname := pref
		funcname, suff2 := parseName(suff[1:])
		name = modname + "." + funcname
		rest = strings.TrimLeftFunc(suff2, unicode.IsSpace)
	} else {
		name = pref
		rest = suff
	}

	return name, rest

}

// Parse a type. This is very stupid and doesn't make much effort to
// be absolutely correct.
//
// SYNTAX: []Type | chan Type | map[Type]Type | *Type | (Type) | Name.Type | Name
func parseType(s string) (Type, string, error) {
	// Array type
	suff, ok := matchPrefix(s, "[]")
	if ok {
		tycon := func(t Type) Type {
			ty := ArrayType{ElementType: t}
			return &ty
		}
		return parseUnaryType(tycon, suff, s)
	}

	// Chan type
	suff, ok = matchPrefix(s, "chan")
	if ok {
		tycon := func(t Type) Type {
			ty := ChanType{ElementType: t}
			return &ty
		}
		return parseUnaryType(tycon, suff, s)
	}

	// Map type
	suff, ok = matchPrefix(s, "map[")
	if ok {
		keyTy, keyRest, keyErr := parseType(suff)
		suff, ok = matchPrefix(keyRest, "]")
		if ok && keyErr == nil {
			tycon := func(t Type) Type {
				ty := MapType{KeyType: keyTy, ValueType: t}
				return &ty
			}
			return parseUnaryType(tycon, keyRest[1:], s)
		}
		return nil, s, fmt.Errorf("Mismatched brackets in '%s'", s)
	}

	// Pointer type
	suff, ok = matchPrefix(s, "*")
	if ok {
		tycon := func(t Type) Type {
			ty := PointerType{TargetType: t}
			return &ty
		}
		return parseUnaryType(tycon, suff, s)
	}

	// Type in (posibly 0) parentheses
	noParens, parenOk := matchDelims(s, "(", ")")
	if parenOk {
		// Basic type OR qualified type
		if noParens == s {
			pref, suff := parseName(s)
			suff = strings.TrimLeftFunc(suff, unicode.IsSpace)

			if len(suff) > 0 && suff[0] == '.' {
				pkg := pref
				tyname, suff2 := parseName(suff[1:])
				ty := BasicType(tyname)
				rest := strings.TrimLeftFunc(suff2, unicode.IsSpace)
				qty := QualifiedType{Package: pkg, Type: &ty}
				return &qty, rest, nil
			}
			basicTy := BasicType(pref)
			rest := suff
			return &basicTy, rest, nil
		}

		return parseType(noParens)
	}

	return nil, s, fmt.Errorf("mismatched parentheses in '%s'", s)
}

// Helper function for parsing a unary type operator: [], chan, or *.
//
// SYNTAX: Type
func parseUnaryType(tycon func(Type) Type, s, orig string) (Type, string, error) {
	var (
		innerTy Type
		rest    string
		err     error
	)

	noSpaces := strings.TrimLeft(s, " ")
	noParens, parenOk := matchDelims(noSpaces, "(", ")")

	if parenOk {
		innerTy, rest, err = parseType(noParens)
	} else {
		err = fmt.Errorf("mismatched parentheses in '%s'", orig)
	}

	return tycon(innerTy), rest, err
}

// Parse a name.
//
// SYNTAX: [a-zA-Z0-9_-]
func parseName(s string) (string, string) {
	name, suff := takeWhileIn(s, "qwertyuiopasdfghjklzxcvbnmQWERTYUIOPASDFGHJKLZXCVBNM1234567890_-")
	rest := strings.TrimLeftFunc(suff, unicode.IsSpace)
	return name, rest
}
