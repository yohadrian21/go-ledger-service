package domain

import (
	"errors"
	"math"
	"testing"
)

func TestParseCurrency(t *testing.T) {
	tests := []struct {
		in      string
		want    Currency
		wantErr bool
	}{
		{in: "USD", want: "USD"},
		{in: " eur ", want: "EUR"},
		{in: "idr", want: "IDR"},
		{in: "", wantErr: true},
		{in: "US", wantErr: true},
		{in: "USDT", wantErr: true},
		{in: "U5D", wantErr: true},
		{in: "ÜSD", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseCurrency(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidCurrency) {
					t.Fatalf("ParseCurrency(%q) error = %v, want ErrInvalidCurrency", tt.in, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseCurrency(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestAddAmountOverflow(t *testing.T) {
	if _, err := addAmount(math.MaxInt64, 1); !errors.Is(err, ErrBalanceOverflow) {
		t.Fatalf("expected overflow, got %v", err)
	}
	if _, err := addAmount(math.MinInt64, -1); !errors.Is(err, ErrBalanceOverflow) {
		t.Fatalf("expected underflow, got %v", err)
	}
	if got, err := addAmount(40, 2); err != nil || got != 42 {
		t.Fatalf("addAmount(40, 2) = %d, %v", got, err)
	}
}
