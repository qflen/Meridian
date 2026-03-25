package query

import (
	"testing"
)

func TestLexerBasicTokens(t *testing.T) {
	tests := []struct {
		input    string
		expected []TokenType
	}{
		{"cpu_usage", []TokenType{TokenIdent, TokenEOF}},
		{"cpu_usage{host=\"web-01\"}", []TokenType{TokenIdent, TokenLBrace, TokenIdent, TokenEQ, TokenString, TokenRBrace, TokenEOF}},
		{"rate(http_requests_total[5m])", []TokenType{TokenIdent, TokenLParen, TokenIdent, TokenLBracket, TokenDuration, TokenRBracket, TokenRParen, TokenEOF}},
		{"sum(x) by (host)", []TokenType{TokenIdent, TokenLParen, TokenIdent, TokenRParen, TokenBy, TokenLParen, TokenIdent, TokenRParen, TokenEOF}},
		{"a + b * 100", []TokenType{TokenIdent, TokenPlus, TokenIdent, TokenMul, TokenNumber, TokenEOF}},
		{"x{a!=\"b\",c=~\"d.*\"}", []TokenType{TokenIdent, TokenLBrace, TokenIdent, TokenNEQ, TokenString, TokenComma, TokenIdent, TokenRE, TokenString, TokenRBrace, TokenEOF}},
		{"x{a!~\"b\"}", []TokenType{TokenIdent, TokenLBrace, TokenIdent, TokenNRE, TokenString, TokenRBrace, TokenEOF}},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			lexer := NewLexer(tc.input)
			tokens, err := lexer.Tokenize()
			if err != nil {
				t.Fatal(err)
			}
			if len(tokens) != len(tc.expected) {
				t.Fatalf("expected %d tokens, got %d: %v", len(tc.expected), len(tokens), tokens)
			}
			for i, tok := range tokens {
				if tok.Type != tc.expected[i] {
					t.Fatalf("token %d: expected %d, got %d (%q)", i, tc.expected[i], tok.Type, tok.Literal)
				}
			}
		})
	}
}

func TestLexerDurations(t *testing.T) {
	tests := []struct {
		input    string
		literal  string
	}{
		{"5m", "5m"},
		{"1h", "1h"},
		{"30s", "30s"},
		{"7d", "7d"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			lexer := NewLexer(tc.input)
			tokens, err := lexer.Tokenize()
			if err != nil {
				t.Fatal(err)
			}
			if tokens[0].Type != TokenDuration {
				t.Fatalf("expected duration, got %d", tokens[0].Type)
			}
			if tokens[0].Literal != tc.literal {
				t.Fatalf("expected %q, got %q", tc.literal, tokens[0].Literal)
			}
		})
	}
}

func TestLexerNumbers(t *testing.T) {
	tests := []struct {
		input   string
		literal string
	}{
		{"42", "42"},
		{"3.14", "3.14"},
		{"100", "100"},
		{"0.5", "0.5"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			lexer := NewLexer(tc.input)
			tokens, err := lexer.Tokenize()
			if err != nil {
				t.Fatal(err)
			}
			if tokens[0].Type != TokenNumber {
				t.Fatalf("expected number, got %d", tokens[0].Type)
			}
			if tokens[0].Literal != tc.literal {
				t.Fatalf("expected %q, got %q", tc.literal, tokens[0].Literal)
			}
		})
	}
}

func TestLexerStringEscape(t *testing.T) {
	lexer := NewLexer(`"hello \"world\""`)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatal(err)
	}
	if tokens[0].Literal != `hello "world"` {
		t.Fatalf("expected escaped string, got %q", tokens[0].Literal)
	}
}

func TestLexerErrors(t *testing.T) {
	tests := []string{
		`"unterminated string`,
		`!invalid`,
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			lexer := NewLexer(input)
			_, err := lexer.Tokenize()
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input string
		ms    int64
	}{
		{"5s", 5000},
		{"5m", 300000},
		{"1h", 3600000},
		{"7d", 604800000},
	}
	for _, tc := range tests {
		d, err := ParseDuration(tc.input)
		if err != nil {
			t.Fatal(err)
		}
		if d.Milliseconds() != tc.ms {
			t.Fatalf("%s: expected %d ms, got %d ms", tc.input, tc.ms, d.Milliseconds())
		}
	}
}
