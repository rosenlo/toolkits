package tree

type TreeNode struct {
	Val   int
	Left  *TreeNode
	Right *TreeNode
}

const NULL = -1 << 16

func Ints2TreeNode(ints []int) *TreeNode {
	n := len(ints)
	if n == 0 {
		return nil
	}
	root := &TreeNode{
		Val: ints[0],
	}
	queue := make([]*TreeNode, 1, n*2)
	queue[0] = root

	for i := 1; i < n; i++ {
		node := queue[0]
		queue = queue[1:]

		if ints[i] != NULL {
			node.Left = &TreeNode{Val: ints[i]}
			queue = append(queue, node.Left)
		}

		i++

		if i < n && ints[i] != NULL {
			node.Right = &TreeNode{Val: ints[i]}
			queue = append(queue, node.Right)
		}
	}

	return root
}

func TreeNode2Ints(root *TreeNode) []int {
	ints := make([]int, 0)
	if root == nil {
		return ints
	}

	queue := []*TreeNode{root}

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		if node == nil {
			if len(queue) > 0 {
				ints = append(ints, NULL)
			}
		} else {
			ints = append(ints, node.Val)
			queue = append(queue, node.Left, node.Right)
		}
	}
	n := len(ints)
	for n > 0 && ints[n-1] == NULL {
		n--
	}
	return ints[:n]
}
