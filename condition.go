package main

import (
	"fmt"
	"strings"
)

// Conditions are evaluated in three classic stages: a lexer splits the text
// into tokens, a recursive descent parser builds a syntax tree, and the tree
// is evaluated against the variables. The language deliberately has no
// function calls and no access to the environment.
//
// Grammar, from lowest to highest precedence:
//
//	or         = and { "or" and }
//	and        = not { "and" not }
//	not        = "not" not | comparison
//	comparison = operand [ ( "==" | "!=" ) operand ]
//	operand    = "(" or ")" | string | "true" | "false" | identifier

const (
	// maxConditionLength bounds the work spent on a single condition.
	maxConditionLength = 4096

	// maxConditionDepth bounds parser recursion so deeply nested input
	// fails with an error instead of exhausting the stack.
	maxConditionDepth = 64
)

type tokenKind int

const (
	tokenEOF tokenKind = iota
	tokenIdent
	tokenString
	tokenLeftParen
	tokenRightParen
	tokenEqual
	tokenNotEqual
)

func (k tokenKind) String() string {
	switch k {
	case tokenEOF:
		return "end of condition"
	case tokenIdent:
		return "identifier"
	case tokenString:
		return "string"
	case tokenLeftParen:
		return "'('"
	case tokenRightParen:
		return "')'"
	case tokenEqual:
		return "'=='"
	case tokenNotEqual:
		return "'!='"
	}

	return "unknown token"
}

type token struct {
	kind tokenKind
	// text is the identifier name or the unquoted string value.
	text string
	// pos is the byte offset of the token in the condition.
	pos int
}

func (t token) String() string {
	switch t.kind {
	case tokenIdent:
		return fmt.Sprintf("%q", t.text)
	case tokenString:
		return fmt.Sprintf("string %q", t.text)
	}

	return t.kind.String()
}

// conditionError reports a problem at a byte offset in the condition.
func conditionError(pos int, format string, args ...any) error {
	return fmt.Errorf("offset %d: %s", pos, fmt.Sprintf(format, args...))
}

// lex splits a condition into tokens. The final token is always tokenEOF.
func lex(input string) ([]token, error) {
	var tokens []token

	for i := 0; i < len(input); {
		c := input[i]

		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++

		case c == '(':
			tokens = append(tokens, token{kind: tokenLeftParen, pos: i})
			i++

		case c == ')':
			tokens = append(tokens, token{kind: tokenRightParen, pos: i})
			i++

		case c == '=' || c == '!':
			if i+1 >= len(input) || input[i+1] != '=' {
				return nil, conditionError(i, "expected == or !=")
			}

			kind := tokenEqual
			if c == '!' {
				kind = tokenNotEqual
			}

			tokens = append(tokens, token{kind: kind, pos: i})
			i += 2

		case c == '\'' || c == '"':
			value, end, err := lexString(input, i)
			if err != nil {
				return nil, err
			}

			tokens = append(tokens, token{kind: tokenString, text: value, pos: i})
			i = end

		case isIdentStart(c):
			start := i
			for i < len(input) && isIdentPart(input[i]) {
				i++
			}

			tokens = append(tokens, token{kind: tokenIdent, text: input[start:i], pos: start})

		default:
			return nil, conditionError(i, "unexpected character %q", rune(c))
		}
	}

	tokens = append(tokens, token{kind: tokenEOF, pos: len(input)})
	return tokens, nil
}

// lexString reads a quoted string starting at input[start] and returns its
// value and the offset after the closing quote. Only the enclosing quote and
// backslash can be escaped, and strings cannot span lines.
func lexString(input string, start int) (string, int, error) {
	quote := input[start]
	var b strings.Builder

	for i := start + 1; i < len(input); i++ {
		c := input[i]

		switch c {
		case quote:
			return b.String(), i + 1, nil

		case '\n', '\r':
			return "", 0, conditionError(start, "unterminated string")

		case '\\':
			i++
			if i >= len(input) || (input[i] != quote && input[i] != '\\') {
				return "", 0, conditionError(i-1, "only quote and backslash escapes are supported")
			}

			b.WriteByte(input[i])

		default:
			b.WriteByte(c)
		}
	}

	return "", 0, conditionError(start, "unterminated string")
}

// node is a parsed condition expression.
type node interface {
	eval(vars map[string]any) (any, error)
}

type literal struct {
	value any
}

type variable struct {
	name string
	pos  int
}

type notExpr struct {
	operand node
	pos     int
}

type logicalExpr struct {
	and         bool
	left, right node
	pos         int
}

type compareExpr struct {
	equal       bool
	left, right node
	pos         int
}

func (n literal) eval(map[string]any) (any, error) {
	return n.value, nil
}

func (n variable) eval(vars map[string]any) (any, error) {
	value, ok := vars[n.name]
	if !ok {
		return nil, conditionError(n.pos, "undefined variable %q (declare it in vars or use --set %s=VALUE)",
			n.name, n.name)
	}

	return value, nil
}

func (n notExpr) eval(vars map[string]any) (any, error) {
	value, err := n.operand.eval(vars)
	if err != nil {
		return nil, err
	}

	b, ok := value.(bool)
	if !ok {
		return nil, conditionError(n.pos, "not operand must be a boolean, got %T", value)
	}

	return !b, nil
}

// eval evaluates both operands without short-circuiting, so a mistake in a
// branch that is currently inactive is still reported.
func (n logicalExpr) eval(vars map[string]any) (any, error) {
	left, err := n.left.eval(vars)
	if err != nil {
		return nil, err
	}

	right, err := n.right.eval(vars)
	if err != nil {
		return nil, err
	}

	a, aok := left.(bool)
	b, bok := right.(bool)
	if !aok || !bok {
		return nil, conditionError(n.pos, "and/or operands must be booleans, got %T and %T", left, right)
	}

	if n.and {
		return a && b, nil
	}

	return a || b, nil
}

func (n compareExpr) eval(vars map[string]any) (any, error) {
	left, err := n.left.eval(vars)
	if err != nil {
		return nil, err
	}

	right, err := n.right.eval(vars)
	if err != nil {
		return nil, err
	}

	if fmt.Sprintf("%T", left) != fmt.Sprintf("%T", right) {
		return nil, conditionError(n.pos, "cannot compare %T with %T", left, right)
	}

	return (left == right) == n.equal, nil
}

type conditionParser struct {
	tokens []token
	next   int
	depth  int
}

func (p *conditionParser) peek() token {
	return p.tokens[p.next]
}

func (p *conditionParser) take() token {
	t := p.tokens[p.next]
	if t.kind != tokenEOF {
		p.next++
	}

	return t
}

func (p *conditionParser) keyword(word string) bool {
	t := p.peek()
	return t.kind == tokenIdent && t.text == word
}

// enter guards recursion; every call must be paired with leave.
func (p *conditionParser) enter() error {
	p.depth++
	if p.depth > maxConditionDepth {
		return conditionError(p.peek().pos, "condition nests deeper than %d levels", maxConditionDepth)
	}

	return nil
}

func (p *conditionParser) leave() {
	p.depth--
}

func (p *conditionParser) or() (node, error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}

	for p.keyword("or") {
		pos := p.take().pos

		right, err := p.and()
		if err != nil {
			return nil, err
		}

		left = logicalExpr{and: false, left: left, right: right, pos: pos}
	}

	return left, nil
}

func (p *conditionParser) and() (node, error) {
	left, err := p.not()
	if err != nil {
		return nil, err
	}

	for p.keyword("and") {
		pos := p.take().pos

		right, err := p.not()
		if err != nil {
			return nil, err
		}

		left = logicalExpr{and: true, left: left, right: right, pos: pos}
	}

	return left, nil
}

func (p *conditionParser) not() (node, error) {
	if !p.keyword("not") {
		return p.comparison()
	}

	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()

	pos := p.take().pos

	operand, err := p.not()
	if err != nil {
		return nil, err
	}

	return notExpr{operand: operand, pos: pos}, nil
}

func (p *conditionParser) comparison() (node, error) {
	left, err := p.operand()
	if err != nil {
		return nil, err
	}

	kind := p.peek().kind
	if kind != tokenEqual && kind != tokenNotEqual {
		return left, nil
	}

	pos := p.take().pos

	right, err := p.operand()
	if err != nil {
		return nil, err
	}

	return compareExpr{equal: kind == tokenEqual, left: left, right: right, pos: pos}, nil
}

func (p *conditionParser) operand() (node, error) {
	t := p.take()

	switch t.kind {
	case tokenLeftParen:
		return p.parenthesized(t)

	case tokenString:
		return literal{value: t.text}, nil

	case tokenIdent:
		switch t.text {
		case "true":
			return literal{value: true}, nil
		case "false":
			return literal{value: false}, nil
		case "and", "or", "not":
			return nil, conditionError(t.pos, "unexpected operator %q", t.text)
		}

		return variable{name: t.text, pos: t.pos}, nil
	}

	return nil, conditionError(t.pos, "expected a variable, boolean, string or parenthesis; got %s", t)
}

func (p *conditionParser) parenthesized(open token) (node, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()

	inner, err := p.or()
	if err != nil {
		return nil, err
	}

	if p.peek().kind != tokenRightParen {
		return nil, conditionError(p.peek().pos, "expected closing parenthesis for '(' at offset %d, got %s",
			open.pos, p.peek())
	}

	p.take()
	return inner, nil
}

// parseCondition parses a non-empty condition into a syntax tree.
func parseCondition(expression string) (node, error) {
	if len(expression) > maxConditionLength {
		return nil, fmt.Errorf("condition exceeds %d bytes", maxConditionLength)
	}

	tokens, err := lex(expression)
	if err != nil {
		return nil, err
	}

	p := &conditionParser{tokens: tokens}

	tree, err := p.or()
	if err != nil {
		return nil, err
	}

	if t := p.peek(); t.kind != tokenEOF {
		return nil, conditionError(t.pos, "unexpected token %s", t)
	}

	return tree, nil
}

// evaluate reports whether a condition holds. An empty condition is true.
func evaluate(expression string, vars map[string]any) (bool, error) {
	if strings.TrimSpace(expression) == "" {
		return true, nil
	}

	tree, err := parseCondition(expression)
	if err != nil {
		return false, err
	}

	value, err := tree.eval(vars)
	if err != nil {
		return false, err
	}

	result, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("condition must evaluate to a boolean, got %T", value)
	}

	return result, nil
}
