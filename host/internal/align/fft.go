package align

import (
	"math"
	"math/bits"
)

// fft is an in-place iterative radix-2 FFT; len(a) must be a power of two.
// inverse computes the unscaled inverse transform.
func fft(a []complex128, inverse bool) {
	n := len(a)
	if n <= 1 {
		return
	}
	shift := 64 - bits.TrailingZeros(uint(n))
	for i := 0; i < n; i++ {
		j := int(bits.Reverse64(uint64(i)) >> shift)
		if j > i {
			a[i], a[j] = a[j], a[i]
		}
	}
	sign := -1.0
	if inverse {
		sign = 1
	}
	for size := 2; size <= n; size <<= 1 {
		ang := sign * 2 * math.Pi / float64(size)
		wn := complex(math.Cos(ang), math.Sin(ang))
		half := size / 2
		for start := 0; start < n; start += size {
			w := complex(1, 0)
			for k := 0; k < half; k++ {
				u := a[start+k]
				v := a[start+k+half] * w
				a[start+k] = u + v
				a[start+k+half] = u - v
				w *= wn
			}
		}
	}
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}
