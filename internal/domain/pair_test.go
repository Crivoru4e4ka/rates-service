package domain

import (
	"errors"
	"testing"
)

func TestParsePair(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		base    string
		quote   string
		wantErr error
	}{
		{name: "обычная пара", in: "EUR/MXN", base: "EUR", quote: "MXN"},
		{name: "нижний регистр и пробелы", in: "  usd/Eur ", base: "USD", quote: "EUR"},
		{name: "без слэша", in: "EURMXN", wantErr: ErrInvalidPair},
		{name: "лишняя часть", in: "EUR/MXN/USD", wantErr: ErrInvalidPair},
		{name: "пустой base", in: "/MXN", wantErr: ErrInvalidPair},
		{name: "короткий код", in: "EU/MXN", wantErr: ErrInvalidPair},
		{name: "цифры в коде", in: "E1R/MXN", wantErr: ErrInvalidPair},
		{name: "одинаковые валюты", in: "EUR/EUR", wantErr: ErrInvalidPair},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pair, err := ParsePair(tt.in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ParsePair(%q): err = %v, want %v", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePair(%q): неожиданная ошибка %v", tt.in, err)
			}
			if pair.Base != tt.base || pair.Quote != tt.quote {
				t.Fatalf("ParsePair(%q) = %s, want %s/%s", tt.in, pair.String(), tt.base, tt.quote)
			}
		})
	}
}

func TestValidRequestID(t *testing.T) {
	if !ValidRequestID("5f0c9e2a-1b7c-4a5e-9d3f-2c6b8a1e4d7f") {
		t.Fatal("валидный UUID не принят")
	}
	for _, id := range []string{
		"",
		"not-a-uuid",
		"5f0c9e2a1b7c4a5e9d3f2c6b8a1e4d7f", // без дефисов
		"5f0c9e2a-1b7c-4a5e-9d3f-2c6b8a1e4d7",
		"5f0c9e2a-1b7c-4a5e-9d3f-2c6b8a1e4d7fx",
	} {
		if ValidRequestID(id) {
			t.Fatalf("ValidRequestID(%q) = true, want false", id)
		}
	}
}
