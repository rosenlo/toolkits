package trees

import (
	"reflect"
	"testing"
)

func TestInts2TreeNode(t *testing.T) {
	tests := [][]int{
		{1, 2, 3},
		{1, 1, 2},
		{1, 2, 3, 4, 5},
		{},
	}
	for i := 0; i < len(tests); i++ {
		node := Ints2TreeNode(tests[i])
		ret := TreeNode2Ints(node)
		t.Log(tests[i], ret)
		if !reflect.DeepEqual(tests[i], ret) {
			t.Fatalf("Wrong Answer, ret: %v right ret: %v", ret, tests[i])
		}
	}
}
