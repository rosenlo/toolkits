package promutil

type Sample struct {
	Metric     map[string]string `json:"metric"`
	Values     []float64         `json:"values"`
	Timestamps []int64           `json:"timestamps"`
}

type QueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Values []any             `json:"values"`
		}
	}
}

type QueryRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric struct {
				InstanceName string `json:"instance_name"`
				Instance     string `json:"instance"`
			}
			Values [][]any `json:"values"`
		}
	}
}

type LabelValuesResponse struct {
	Status string   `json:"status"`
	Data   []string `json:"data"`
}
