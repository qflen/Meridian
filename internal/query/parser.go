package query

import (
	"fmt"
	"strconv"
)

// Parser builds an AST from a token stream.
type Parser struct {
	tokens []Token
	pos    int
}

// aggregation operator names recognized by the parser.
var aggregateOps = map[string]bool{
	"sum": true, "avg": true, "max": true, "min": true, "count": true,
	"topk": true, "bottomk": true,
}

// function names recognized by the parser.
var functionNames = map[string]bool{
	"rate": true, "avg": true, "sum": true, "max": true, "min": true,
	"count": true, "histogram_quantile": true,
}

// Parse parses a PromQL-subset expression string into an AST.
func Parse(input string) (Expr, error) {
	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, err
	}
	p := &Parser{tokens: tokens}
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().Type != TokenEOF {
		return nil, fmt.Errorf("unexpected token %q at position %d", p.peek().Literal, p.peek().Pos)
	}
	return expr, nil
}

func (p *Parser) parseExpr() (Expr, error) {
	return p.parseBinaryExpr(0)
}

func (p *Parser) parseBinaryExpr(minPrec int) (Expr, error) {
	left, err := p.parseUnaryExpr()
	if err != nil {
		return nil, err
	}

	for {
		tok := p.peek()
		prec := precedence(tok.Type)
		if prec < minPrec {
			break
		}
		op := tok.Literal
		p.advance()

		right, err := p.parseBinaryExpr(prec + 1)
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: op, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseUnaryExpr() (Expr, error) {
	tok := p.peek()

	switch tok.Type {
	case TokenNumber:
		p.advance()
		val, err := strconv.ParseFloat(tok.Literal, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid number %q: %w", tok.Literal, err)
		}
		return &NumberLiteral{Value: val}, nil

	case TokenIdent:
		// Could be: function call, aggregate, or vector selector
		if aggregateOps[tok.Literal] {
			return p.parseAggregateOrFunction()
		}
		if functionNames[tok.Literal] && p.peekAt(1).Type == TokenLParen {
			return p.parseFunctionCall()
		}
		return p.parseVectorOrRange()

	case TokenMinus, TokenPlus:
		// Unary sign on the operand that follows.
		p.advance()
		operand, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		if tok.Type == TokenPlus {
			return operand, nil
		}
		// Fold a literal; otherwise negate the vector via 0 - operand.
		if nl, ok := operand.(*NumberLiteral); ok {
			return &NumberLiteral{Value: -nl.Value}, nil
		}
		return &BinaryExpr{Op: "-", Left: &NumberLiteral{Value: 0}, Right: operand}, nil

	case TokenLBrace:
		// Bare label-only selector, e.g. {job="x"}.
		return p.finishSelector("")

	case TokenLParen:
		p.advance()
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().Type != TokenRParen {
			return nil, fmt.Errorf("expected ')' at position %d", p.peek().Pos)
		}
		p.advance()
		return expr, nil
	}

	return nil, fmt.Errorf("unexpected token %q at position %d", tok.Literal, tok.Pos)
}

func (p *Parser) parseAggregateOrFunction() (Expr, error) {
	name := p.peek().Literal
	p.advance()

	// Grouping can precede the argument list: avg by (labels) (expr).
	if p.peek().Type == TokenBy || p.peek().Type == TokenWithout {
		without := p.peek().Type == TokenWithout
		p.advance()
		grouping, err := p.parseGrouping()
		if err != nil {
			return nil, err
		}
		expr, err := p.parseAggregateArgs(name)
		if err != nil {
			return nil, err
		}
		return applyGrouping(expr, grouping, without)
	}

	if p.peek().Type == TokenLParen {
		expr, err := p.parseAggregateArgs(name)
		if err != nil {
			return nil, err
		}
		// Grouping can also follow the argument list: sum(expr) without (labels).
		if p.peek().Type == TokenBy || p.peek().Type == TokenWithout {
			without := p.peek().Type == TokenWithout
			p.advance()
			grouping, err := p.parseGrouping()
			if err != nil {
				return nil, err
			}
			return applyGrouping(expr, grouping, without)
		}
		return expr, nil
	}

	// Bare aggregate name without parens? That's a metric name.
	return p.finishSelector(name)
}

// parseAggregateArgs parses the ( arg [, arg] ) following an aggregate or
// function name. Two arguments mean a parameterized aggregate such as
// topk(k, v); one argument yields an aggregate or a function call.
func (p *Parser) parseAggregateArgs(name string) (Expr, error) {
	if p.peek().Type != TokenLParen {
		return nil, fmt.Errorf("expected '(' after %q at position %d", name, p.peek().Pos)
	}
	p.advance()

	first, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	if p.peek().Type == TokenComma {
		p.advance()
		second, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().Type != TokenRParen {
			return nil, fmt.Errorf("expected ')' at position %d", p.peek().Pos)
		}
		p.advance()
		return &AggregateExpr{Op: name, Param: first, Expr: second}, nil
	}

	if p.peek().Type != TokenRParen {
		return nil, fmt.Errorf("expected ')' at position %d", p.peek().Pos)
	}
	p.advance()

	if aggregateOps[name] && !onlyFunctionNames[name] {
		return &AggregateExpr{Op: name, Expr: first}, nil
	}
	return &FunctionCall{Name: name, Args: []Expr{first}}, nil
}

// applyGrouping attaches a by()/without() clause to an aggregate expression.
func applyGrouping(expr Expr, grouping []string, without bool) (Expr, error) {
	ae, ok := expr.(*AggregateExpr)
	if !ok {
		return nil, fmt.Errorf("grouping clause is only valid on an aggregation")
	}
	ae.Grouping = grouping
	ae.Without = without
	return ae, nil
}

var onlyFunctionNames = map[string]bool{
	"rate": true, "histogram_quantile": true,
}

func (p *Parser) parseFunctionCall() (Expr, error) {
	name := p.peek().Literal
	p.advance()

	if p.peek().Type != TokenLParen {
		return nil, fmt.Errorf("expected '(' after function name %q", name)
	}
	p.advance()

	var args []Expr
	if p.peek().Type != TokenRParen {
		for {
			arg, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
			if p.peek().Type != TokenComma {
				break
			}
			p.advance()
		}
	}

	if p.peek().Type != TokenRParen {
		return nil, fmt.Errorf("expected ')' at position %d", p.peek().Pos)
	}
	p.advance()

	return &FunctionCall{Name: name, Args: args}, nil
}

func (p *Parser) parseVectorOrRange() (Expr, error) {
	name := p.peek().Literal
	p.advance()
	return p.finishSelector(name)
}

// finishSelector parses the matcher braces (if any) for the given metric name
// and an optional trailing range [duration]. name is "" for a bare selector.
func (p *Parser) finishSelector(name string) (Expr, error) {
	vs, err := p.parseVectorSelectorFrom(name)
	if err != nil {
		return nil, err
	}

	// Check for range selector [duration]
	if p.peek().Type == TokenLBracket {
		p.advance()
		if p.peek().Type != TokenDuration {
			return nil, fmt.Errorf("expected duration in range selector at position %d", p.peek().Pos)
		}
		dur, err := ParseDuration(p.peek().Literal)
		if err != nil {
			return nil, err
		}
		p.advance()
		if p.peek().Type != TokenRBracket {
			return nil, fmt.Errorf("expected ']' at position %d", p.peek().Pos)
		}
		p.advance()
		return &RangeSelector{Vector: vs, Duration: dur}, nil
	}

	return vs, nil
}

func (p *Parser) parseVectorSelectorFrom(name string) (*VectorSelector, error) {
	vs := &VectorSelector{Name: name}

	if p.peek().Type == TokenLBrace {
		p.advance()
		for p.peek().Type != TokenRBrace {
			if len(vs.Matchers) > 0 {
				if p.peek().Type != TokenComma {
					return nil, fmt.Errorf("expected ',' or '}' in label matchers at position %d", p.peek().Pos)
				}
				p.advance()
			}

			m, err := p.parseMatcher()
			if err != nil {
				return nil, err
			}
			vs.Matchers = append(vs.Matchers, m)
		}
		p.advance() // skip '}'
	}

	return vs, nil
}

func (p *Parser) parseMatcher() (Matcher, error) {
	if p.peek().Type != TokenIdent {
		return Matcher{}, fmt.Errorf("expected label name at position %d", p.peek().Pos)
	}
	name := p.peek().Literal
	p.advance()

	var matchType MatcherType
	switch p.peek().Type {
	case TokenEQ:
		matchType = MatcherEqual
	case TokenNEQ:
		matchType = MatcherNotEqual
	case TokenRE:
		matchType = MatcherRegexp
	case TokenNRE:
		matchType = MatcherNotRegexp
	default:
		return Matcher{}, fmt.Errorf("expected matcher operator at position %d", p.peek().Pos)
	}
	p.advance()

	if p.peek().Type != TokenString {
		return Matcher{}, fmt.Errorf("expected string value at position %d, got %q", p.peek().Pos, p.peek().Literal)
	}
	value := p.peek().Literal
	p.advance()

	return Matcher{Name: name, Value: value, Type: matchType}, nil
}

func (p *Parser) parseGrouping() ([]string, error) {
	if p.peek().Type != TokenLParen {
		return nil, fmt.Errorf("expected '(' after 'by' at position %d", p.peek().Pos)
	}
	p.advance()

	var labels []string
	for p.peek().Type != TokenRParen {
		if len(labels) > 0 {
			if p.peek().Type != TokenComma {
				return nil, fmt.Errorf("expected ',' in grouping at position %d", p.peek().Pos)
			}
			p.advance()
		}
		if p.peek().Type != TokenIdent {
			return nil, fmt.Errorf("expected label name in grouping at position %d", p.peek().Pos)
		}
		labels = append(labels, p.peek().Literal)
		p.advance()
	}
	p.advance() // skip ')'
	return labels, nil
}

func (p *Parser) peek() Token {
	if p.pos >= len(p.tokens) {
		return Token{Type: TokenEOF}
	}
	return p.tokens[p.pos]
}

func (p *Parser) peekAt(offset int) Token {
	idx := p.pos + offset
	if idx >= len(p.tokens) {
		return Token{Type: TokenEOF}
	}
	return p.tokens[idx]
}

func (p *Parser) advance() {
	p.pos++
}

func precedence(t TokenType) int {
	switch t {
	case TokenPlus, TokenMinus:
		return 1
	case TokenMul, TokenDiv:
		return 2
	default:
		return -1
	}
}
