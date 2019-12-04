package mathutil

import "math"

// Mean return the mean of float64 values.
func Mean(values []float64) float64 {
	var sum float64
	for i := 0; i < len(values); i++ {
		sum += values[i]
	}
	return sum / float64(len(values))
}

// StdDev return the standard deviation of float64 values.
func StdDev(values []float64) float64 {
	var sum float64
	mean := Mean(values)
	for i := 0; i < len(values); i++ {
		temp := values[i] - mean
		sum += temp * temp
	}
	return math.Sqrt(sum / float64(len(values)-1))
}
