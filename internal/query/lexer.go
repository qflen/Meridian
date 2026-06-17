package query

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/meridiandb/meridian/internal/config"
)

// TokenType identifies the type of a lexed token.
type TokenType int

const (
	// TokenEOF signals the end of input.
	TokenEOF TokenType = iota
	// TokenIdent is an identifier (metric name, function name, keyword).
	TokenIdent
	// TokenString is a quoted string literal.
	TokenString
	// TokenNumber is a numeric literal.
	TokenNumber
	// TokenDuration is a duration literal like 5m or 1h.
	TokenDuration
	// TokenLBrace is '{'.
	TokenLBrace
	// TokenRBrace is '}'.
	TokenRBrace
	// TokenLBracket is '['.
	TokenLBracket
	// TokenRBracket is ']'.
	TokenRBracket
	// TokenLParen is '('.
	TokenLParen
	// TokenRParen is ')'.
	TokenRParen
	// TokenComma is ','.
	TokenComma
	// TokenEQ is '='.
	TokenEQ
	// TokenNEQ is '!='.
	TokenNEQ
	// TokenRE is '=~'.
	TokenRE
	// TokenNRE is '!~'.
	TokenNRE
	// TokenBy is the 'by' keyword.
	TokenBy
	// TokenPlus is '+'.
	TokenPlus
	// TokenMinus is '-'.
	TokenMinus
	// TokenMul is '*'.
	TokenMul
	// TokenDiv is '/'.
	TokenDiv
)

// Token represents a single lexical token.
type Token struct {
	Type    TokenType
	Literal string
	Pos     int
}

// Lexer tokenizes a PromQL-subset input string.
type Lexer struct {
	input  string
	pos    int
	tokens []Token
}

// NewLexer creates a new lexer for the given input.
func NewLexer(input string) *Lexer {
	return &Lexer{input: input}
}

// Tokenize processes the entire input and returns all tokens.
func (l *Lexer) Tokenize() ([]Token, error) {
	for {
		tok, err := l.next()
		if err != nil {
			return nil, err
		}
		l.tokens = append(l.tokens, tok)
		if tok.Type == TokenEOF {
			break
		}
	}
	return l.tokens, nil
}

func (l *Lexer) next() (Token, error) {
	l.skipWhitespace()
	if l.pos >= len(l.input) {
		return Token{Type: TokenEOF, Pos: l.pos}, nil
	}

	ch := l.input[l.pos]
	startPos := l.pos

	switch ch {
	case '{':
		l.pos++
		return Token{Type: TokenLBrace, Literal: "{", Pos: startPos}, nil
	case '}':
		l.pos++
		return Token{Type: TokenRBrace, Literal: "}", Pos: startPos}, nil
	case '[':
		l.pos++
		return Token{Type: TokenLBracket, Literal: "[", Pos: startPos}, nil
	case ']':
		l.pos++
		return Token{Type: TokenRBracket, Literal: "]", Pos: startPos}, nil
	case '(':
		l.pos++
		return Token{Type: TokenLParen, Literal: "(", Pos: startPos}, nil
	case ')':
		l.pos++
		return Token{Type: TokenRParen, Literal: ")", Pos: startPos}, nil
	case ',':
		l.pos++
		return Token{Type: TokenComma, Literal: ",", Pos: startPos}, nil
	case '+':
		l.pos++
		return Token{Type: TokenPlus, Literal: "+", Pos: startPos}, nil
	case '-':
		l.pos++
		return Token{Type: TokenMinus, Literal: "-", Pos: startPos}, nil
	case '*':
		l.pos++
		return Token{Type: TokenMul, Literal: "*", Pos: startPos}, nil
	case '/':
		l.pos++
		return Token{Type: TokenDiv, Literal: "/", Pos: startPos}, nil
	case '=':
		l.pos++
		if l.pos < len(l.input) && l.input[l.pos] == '~' {
			l.pos++
			return Token{Type: TokenRE, Literal: "=~", Pos: startPos}, nil
		}
		return Token{Type: TokenEQ, Literal: "=", Pos: startPos}, nil
	case '!':
		l.pos++
		if l.pos < len(l.input) {
			switch l.input[l.pos] {
			case '=':
				l.pos++
				return Token{Type: TokenNEQ, Literal: "!=", Pos: startPos}, nil
			case '~':
				l.pos++
				return Token{Type: TokenNRE, Literal: "!~", Pos: startPos}, nil
			}
		}
		return Token{}, fmt.Errorf("unexpected character '!' at position %d", startPos)
	case '"':
		return l.readString()
	}

	if ch == '.' || (ch >= '0' && ch <= '9') {
		return l.readNumber()
	}

	if isIdentStart(ch) {
		return l.readIdent()
	}

	return Token{}, fmt.Errorf("unexpected character %q at position %d", ch, startPos)
}

func (l *Lexer) readString() (Token, error) {
	startPos := l.pos
	l.pos++ // skip opening quote
	var sb strings.Builder
	for l.pos < len(l.input) {
		ch := l.input[l.pos]
		if ch == '\\' && l.pos+1 < len(l.input) {
			l.pos++
			sb.WriteByte(l.input[l.pos])
			l.pos++
			continue
		}
		if ch == '"' {
			l.pos++
			return Token{Type: TokenString, Literal: sb.String(), Pos: startPos}, nil
		}
		sb.WriteByte(ch)
		l.pos++
	}
	return Token{}, fmt.Errorf("unterminated string at position %d", startPos)
}

// consumeNumber advances over a run of digits with at most one decimal point.
func (l *Lexer) consumeNumber() {
	hasDot := false
	for l.pos < len(l.input) {
		ch := l.input[l.pos]
		if ch == '.' {
			if hasDot {
				break
			}
			hasDot = true
			l.pos++
			continue
		}
		if ch >= '0' && ch <= '9' {
			l.pos++
			continue
		}
		break
	}
}

func (l *Lexer) readNumber() (Token, error) {
	startPos := l.pos
	l.consumeNumber()

	// A duration is one or more <number><unit> groups: 5m, 1h30m, 1.5h, 7d.
	if l.pos < len(l.input) && isDurationSuffix(l.input[l.pos]) {
		return l.readDuration(startPos)
	}

	return Token{Type: TokenNumber, Literal: l.input[startPos:l.pos], Pos: startPos}, nil
}

// readDuration consumes a full compound duration starting just after the first
// numeric part (already consumed). It accepts repeated <number><unit> groups so
// "1h30m" becomes a single token rather than two.
func (l *Lexer) readDuration(startPos int) (Token, error) {
	for {
		if l.pos >= len(l.input) || !isDurationSuffix(l.input[l.pos]) {
			return Token{}, fmt.Errorf("invalid duration %q at position %d", l.input[startPos:l.pos], startPos)
		}
		l.pos++ // consume the unit
		// Continue only if another <number><unit> group follows.
		if l.pos < len(l.input) && l.input[l.pos] >= '0' && l.input[l.pos] <= '9' {
			l.consumeNumber()
			continue
		}
		break
	}
	return Token{Type: TokenDuration, Literal: l.input[startPos:l.pos], Pos: startPos}, nil
}

func (l *Lexer) readIdent() (Token, error) {
	startPos := l.pos
	for l.pos < len(l.input) && isIdentChar(l.input[l.pos]) {
		l.pos++
	}
	lit := l.input[startPos:l.pos]

	if lit == "by" {
		return Token{Type: TokenBy, Literal: lit, Pos: startPos}, nil
	}

	return Token{Type: TokenIdent, Literal: lit, Pos: startPos}, nil
}

func (l *Lexer) skipWhitespace() {
	for l.pos < len(l.input) && unicode.IsSpace(rune(l.input[l.pos])) {
		l.pos++
	}
}

func isIdentStart(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func isIdentChar(ch byte) bool {
	return isIdentStart(ch) || (ch >= '0' && ch <= '9') || ch == ':' || ch == '.'
}

func isDurationSuffix(ch byte) bool {
	return ch == 's' || ch == 'm' || ch == 'h' || ch == 'd' || ch == 'w'
}

// ParseDuration parses a PromQL duration string like "5m", "1h30m", or "1.5h".
// It delegates to config.ParseDuration so the query and config layers share one
// duration grammar (compound units, decimals, and the d/w suffixes that Go's
// time.ParseDuration does not understand).
func ParseDuration(s string) (time.Duration, error) {
	return config.ParseDuration(s)
}
