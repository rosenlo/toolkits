package common

import (
	"encoding/json"

	"github.com/rosenlo/toolkits/structure/maps"
)

func Contains(slice []int, item int) bool {
	set := make(map[int]struct{}, len(slice))
	for _, s := range slice {
		set[s] = struct{}{}
	}
	_, ok := set[item]
	return ok
}

func DuplicateRemove(slice []string) []string {
	users := maps.NewStringMap()
	for _, element := range slice {
		users.Add(element)
	}
	return users.ToSlice()
}

func ToJSON(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
