package stack

import (
	"errors"
)

type Node struct {
	Data interface{}
}

type Stack struct {
	Nodes    []*Node
	Top      int
	MaxStack int
}

func (s *Stack) Empty() (bool, error) {
	if s.Top == 0 {
		return true, errors.New("The stack is empty")
	}
	return false, nil
}

func (s *Stack) Length() int {
	return s.Top
}

func (s *Stack) GetTop() (*Node, error) {
	empty, err := s.Empty()
	if empty {
		return nil, err
	}
	return s.Nodes[s.Top-1], nil
}

func (s *Stack) Push(e interface{}) error {
	if s.Top == s.MaxStack {
		return errors.New("overflow")
	}
	// fmt.Printf("Push -> %s\n", e)
	s.Nodes[s.Top] = &Node{Data: e}
	s.Top++
	return nil
}

func (s *Stack) Pop() (*Node, error) {
	empty, err := s.Empty()
	if empty {
		return nil, err
	}
	data := s.Nodes[s.Top-1]
	s.Nodes = s.Nodes[0 : s.Top-1]
	s.Top--
	//.fmt.Printf("Pop -> %s\n", data)
	return data, nil
}

func NewStack(length int) *Stack {
	return &Stack{
		Nodes:    make([]*Node, length),
		Top:      0,
		MaxStack: length,
	}
}
