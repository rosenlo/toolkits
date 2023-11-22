package trie

type Node struct {
	children map[rune]*Node
	isEnd    bool
}

type Trie struct {
	root *Node
}

func New() *Trie {
	return &Trie{NewNode()}
}

func NewNode() *Node {
	node := new(Node)
	node.children = make(map[rune]*Node)
	node.isEnd = false
	return node
}

// Insert word into the Trie.
func (t *Trie) Insert(word string) {
	current := t.root
	for _, c := range word {
		if _, ok := current.children[c]; !ok {
			current.children[c] = NewNode()
		}
		current = current.children[c]
	}
	current.isEnd = true
}

// StartsWith Check if the given word can be matched by a white-listed prefix.
func (t *Trie) StartsWith(word string) bool {
	current := t.root
	for _, c := range word {
		if _, ok := current.children[c]; !ok {
			return false
		}
		current = current.children[c]
		if current.isEnd {
			return true
		}
	}
	return false
}

// Search Check if the given word can be matched fully.
func (t *Trie) Search(word string) bool {
	current := t.root
	for _, c := range word {
		if current.children[c] == nil {
			return false
		}
		current = current.children[c]
	}
	return current.isEnd
}
