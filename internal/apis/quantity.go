package apis

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// Quantity is a resource amount such as "500m" or "256Mi". It is kept in the
// form the client wrote; Value and MilliValue compute the number.
type Quantity string

var quantitySuffix = map[string]*big.Rat{
	"n": big.NewRat(1, 1e9), "u": big.NewRat(1, 1e6), "m": big.NewRat(1, 1e3),
	"":  big.NewRat(1, 1),
	"k": big.NewRat(1e3, 1), "M": big.NewRat(1e6, 1), "G": big.NewRat(1e9, 1),
	"T": big.NewRat(1e12, 1), "P": big.NewRat(1e15, 1), "E": big.NewRat(1e18, 1),
	"Ki": big.NewRat(1<<10, 1), "Mi": big.NewRat(1<<20, 1), "Gi": big.NewRat(1<<30, 1),
	"Ti": big.NewRat(1<<40, 1), "Pi": big.NewRat(1<<50, 1), "Ei": big.NewRat(1<<60, 1),
}

// ParseQuantity returns the exact value of s.
func ParseQuantity(s string) (*big.Rat, error) {
	bad := fmt.Errorf("quantity %q must be a number with an optional suffix (m, k, M, G, Ki, Mi, Gi, ...)", s)
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits, dots := 0, 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		if s[i] == '.' {
			dots++
		} else {
			digits++
		}
		i++
	}
	if digits == 0 || dots > 1 {
		return nil, bad
	}
	num, suffix := s[:i], s[i:]
	v, ok := new(big.Rat).SetString(num)
	if !ok {
		return nil, bad
	}
	if mul, ok := quantitySuffix[suffix]; ok {
		return v.Mul(v, mul), nil
	}
	// decimal exponent: 1e3, 5E-1
	if (suffix[0] == 'e' || suffix[0] == 'E') && len(suffix) > 1 && len(suffix) <= 5 {
		exp := strings.TrimLeft(suffix[1:], "+-")
		if exp != "" && strings.Trim(exp, "0123456789") == "" {
			if e, ok := new(big.Rat).SetString("1e" + suffix[1:]); ok {
				return v.Mul(v, e), nil
			}
		}
	}
	return nil, bad
}

// scaled returns the value multiplied by scale, rounded up.
func (q Quantity) scaled(scale int64) (int64, error) {
	v, err := ParseQuantity(string(q))
	if err != nil {
		return 0, err
	}
	v.Mul(v, big.NewRat(scale, 1))
	n := new(big.Int).Quo(v.Num(), v.Denom()) // truncates toward zero
	if v.Sign() > 0 && !v.IsInt() {
		n.Add(n, big.NewInt(1))
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("quantity %q is out of range", string(q))
	}
	return n.Int64(), nil
}

func (q Quantity) Value() (int64, error)      { return q.scaled(1) }
func (q Quantity) MilliValue() (int64, error) { return q.scaled(1000) }

func (q Quantity) MarshalJSON() ([]byte, error) { return json.Marshal(string(q)) }

// UnmarshalJSON accepts both "1" and 1, as YAML manifests commonly write numbers.
func (q *Quantity) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	if _, err := ParseQuantity(s); err != nil {
		return err
	}
	*q = Quantity(s)
	return nil
}
