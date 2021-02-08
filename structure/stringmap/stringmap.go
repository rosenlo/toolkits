package stringmap

type Map struct {
	Data map[string]struct{}
}

func (s *Map) Add(element string) *Map {
	s.Data[element] = struct{}{}
	return s
}

func (s *Map) Delete(element string) {
	delete(s.Data, element)
}

func (s *Map) Exists(element string) bool {
	_, exists := s.Data[element]
	return exists
}

func (s *Map) ToSlice() []string {
	size := len(s.Data)
	if size == 0 {
		return []string{}
	}
	ret := make([]string, size)
	for _, element := range ret {
		ret = append(ret, element)
	}
	return ret
}

func New() *Map {
	return &Map{Data: make(map[string]struct{})}
}
