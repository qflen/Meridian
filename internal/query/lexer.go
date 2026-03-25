package query

import (
	"fmt"
	"strings"
	"time"
	"unicode"
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

func (l *Lexer) readNumber() (Token, error) {
	startPos := l.pos
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

	lit := l.input[startPos:l.pos]

	// Check if it's a duration (number followed by s/m/h/d)
	if l.pos < len(l.input) && isDurationSuffix(l.input[l.pos]) && !hasDot {
		return l.readDuration(startPos, lit)
	}

	return Token{Type: TokenNumber, Literal: lit, Pos: startPos}, nil
}

func (l *Lexer) readDuration(startPos int, numPart string) (Token, error) {
	suffix := l.input[l.pos]
	l.pos++
	lit := numPart + string(suffix)
	return Token{Type: TokenDuration, Literal: lit, Pos: startPos}, nil
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
	return ch == 's' || ch == 'm' || ch == 'h' || ch == 'd'
}

// ParseDuration parses a PromQL duration string like "5m" or "1h30m".
func ParseDuration(s string) (time.Duration, error) {
	if len(s) == 0 {
		return 0, fmt.Errorf("empty duration")
	}
	last := s[len(s)-1]
	numStr := s[:len(s)-1]

	var multiplier time.Duration
	switch last {
	case 's':
		multiplier = time.Second
	case 'm':
		multiplier = time.Minute
	case 'h':
		multiplier = time.Hour
	case 'd':
		multiplier = 24 * time.Hour
	default:
		return 0, fmt.Errorf("unknown duration suffix: %c", last)
	}

	var n int
	for _, ch := range numStr {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid duration: %s", s)
		}
		n = n*10 + int(ch-'0')
	}
	return time.Duration(n) * multiplier, nil
}
