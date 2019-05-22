package maps

type StringMap struct {
	Data map[string]struct{}
}

func (s *StringMap) Add(element string) *StringMap {
	s.Data[element] = struct{}{}
	return s
}

func (s *StringMap) Delete(element string) {
	delete(s.Data, element)
}

func (s *StringMap) Exists(element string) bool {
	_, exists := s.Data[element]
	return exists
}

func (s *StringMap) ToSlice() []string {
	length := len(s.Data)
	if length == 0 {
		return []string{}
	}
	ret := make([]string, length)
	for _, element := range ret {
		ret = append(ret, element)
	}
	return ret
}

func NewStringMap() *StringMap {
	return &StringMap{Data: make(map[string]struct{})}
}
