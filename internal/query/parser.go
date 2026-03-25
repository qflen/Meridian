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

	if p.peek().Type == TokenLParen {
		// Could be function call: sum(expr) or aggregate: sum(expr) by (labels)
		p.advance()
		arg, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().Type != TokenRParen {
			return nil, fmt.Errorf("expected ')' at position %d", p.peek().Pos)
		}
		p.advance()

		// Check for "by" clause
		if p.peek().Type == TokenBy {
			p.advance()
			grouping, err := p.parseGrouping()
			if err != nil {
				return nil, err
			}
			return &AggregateExpr{Op: name, Expr: arg, Grouping: grouping}, nil
		}

		// If it's an aggregate op without "by", treat as aggregate
		if aggregateOps[name] && !onlyFunctionNames[name] {
			return &AggregateExpr{Op: name, Expr: arg}, nil
		}

		return &FunctionCall{Name: name, Args: []Expr{arg}}, nil
	}

	// Bare aggregate name without parens? That's a metric name.
	return p.parseVectorSelectorFrom(name)
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
