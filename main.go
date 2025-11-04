package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const objectsDir string = ".git/objects/"
const nullOperator string = "\x00"
const authorString string = "Kunal Vardey <vardeyk@gmail.com>"
const committerString string = "Kunal Vardey <vardeyk@gmail.com>"

// Info holds the metadata for a file or directory.
type Info struct {
	Size      int      // Size of the file in bytes (0 for directories)
	FileMode  string   // git object file mode
	HashBytes [20]byte // Content hash (20 bytes, e.g., SHA-1), initially empty
	IsDir     bool     // True if it is a directory, false if it is a file
	FullPath  string   // Full path of the file or directory
}

// Node represents a single file or directory in the tree.
type Node struct {
	Name     string
	Info     Info
	Parent   *Node
	Children map[string]*Node // Map of child names to child Node pointers
}

// Tree represents the entire file system structure, starting from the root.
type Tree struct {
	Root *Node
}

type TreeEntry struct {
	Name   string        // name of file or dir
	Buffer *bytes.Buffer // The data
}

// NewTree initializes a new Tree with a root directory.
func NewTree(srcDir string) *Tree {
	// The root node is always a directory and has no name in the context of the path.
	root := NewNode(srcDir, true, 0, "40000", nil, srcDir)
	return &Tree{Root: root}
}

// NewNode is a constructor for creating a new Node.
func NewNode(name string, isDir bool, size int, mode string, parent *Node, fullPath string) *Node {
	// Only files should have a non-zero size.
	if isDir {
		size = 0
	}

	return &Node{
		Name: name,
		Info: Info{
			Size:      size,
			FileMode:  mode,
			HashBytes: [20]byte{}, // Initializes to all zero bytes
			IsDir:     isDir,
			FullPath:  fullPath,
		},
		Parent:   parent,
		Children: make(map[string]*Node),
	}
}

func gitObjReaderHelper(inputSha string) []byte {
	dir := objectsDir + inputSha[:2]
	fileName := inputSha[2:]
	fullPath := dir + "/" + fileName

	f, err := os.OpenFile(fullPath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file: %s\n", err)
		os.Exit(1)
	}
	defer f.Close()

	r, err := zlib.NewReader(f)
	if err != nil {
		panic(err)
	}
	defer r.Close()
	decompressedBytes, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}

	return decompressedBytes
}

func createBlobObjectFromFilePath(filePath string) [20]byte {
	fileBytes, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file: %s\n", err)
		return [20]byte{}
	}
	return hashObject("blob", fileBytes)
}

// getRelativePath returns the path relative to the source directory
func getRelativePath(sourcePath, fullPath string) (string, error) {
	relPath, err := filepath.Rel(sourcePath, fullPath)
	if err != nil {
		return "", err
	}
	if relPath == "." {
		return "", nil
	}
	return relPath, nil
}

// Insert inserts a new file or directory into the tree based on its full path.
// It creates any necessary parent directories along the way.
func (t *Tree) Insert(sourcePath, fullPath string, isDir bool, size int, mode string) (*Node, error) {
	// Get the path relative to the source directory
	relPath, err := getRelativePath(sourcePath, fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get relative path: %w", err)
	}

	if relPath == "" {
		// This is the root directory
		return t.Root, nil
	}

	// Clean the path to handle redundancies and trim separators
	cleanedPath := filepath.Clean(relPath)
	cleanedPath = strings.Trim(cleanedPath, string(os.PathSeparator))

	// Split the path into segments (names)
	parts := strings.Split(cleanedPath, string(os.PathSeparator))

	if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		return nil, fmt.Errorf("invalid or empty path: %s", fullPath)
	}

	currentNode := t.Root

	// Traverse and create intermediate directory nodes
	for i, name := range parts {
		if name == "" || name == "." {
			continue // Skip empty or current directory references
		}

		isLastPart := (i == len(parts)-1)

		if child, exists := currentNode.Children[name]; exists {
			// Node already exists, continue traversal
			if isLastPart && child.Info.IsDir != isDir {
				return nil, fmt.Errorf("conflict: path %s already exists but its type (%t) conflicts with new type (%t)", fullPath, child.Info.IsDir, isDir)
			}
			currentNode = child
		} else {
			// Node does not exist, create it
			var newNode *Node
			if isLastPart {
				// This is the final file/directory being inserted
				newNode = NewNode(name, isDir, size, mode, currentNode, fullPath)

				//Calculate hash for files
				if !isDir {
					newNode.Info.HashBytes = createBlobObjectFromFilePath(fullPath)
				}
			} else {
				// Intermediate node must be a directory
				newNode = NewNode(name, true, 0, "40000", currentNode, fullPath)
			}

			currentNode.Children[name] = newNode
			currentNode = newNode
		}
	}

	return currentNode, nil
}

// getGitMode determines the 6-digit Git mode string for a given file entry.
func getGitObjectMode(d fs.DirEntry) (string, error) {
	// 1. Check for Directory
	if d.IsDir() {
		return "40000", nil // Git mode for a tree object (directory)
	}

	// 2. Check for Symbolic Link
	if d.Type()&fs.ModeSymlink != 0 {
		return "120000", nil // Git mode for a symbolic link
	}

	// 3. Handle Regular Files (Blob Objects)
	if d.Type().IsRegular() {
		// Need full os.FileInfo to check for the executable bit
		info, err := d.Info()
		if err != nil {
			return "", err
		}

		// Check the executable bit:
		// 0111 (octal) represents the executable bits (user, group, other)
		// If ANY of the executable bits are set, Git treats it as an executable file.
		if info.Mode()&0111 != 0 {
			return "100755", nil // Executable file
		}

		return "100644", nil // Non-executable file
	}

	// For any other special file types (like devices, sockets, etc.), Git ignores them
	return "", fmt.Errorf("unsupported file type for Git: %s", d.Type().String())
}

func catFile(inputSha string) {
	decompressedBytes := gitObjReaderHelper(inputSha)
	content := strings.Split(string(decompressedBytes), nullOperator)[1]
	// this will print the contents
	fmt.Fprintf(os.Stdout, "%s", content)
	os.Exit(0)
}

func zlibCompressBytes(contentString string) *bytes.Buffer {
	compressedBytes := new(bytes.Buffer)

	compressedBytesWriter := zlib.NewWriter(compressedBytes)
	_, err := compressedBytesWriter.Write([]byte(contentString))

	if err != nil {
		panic(err)
	}

	if err := compressedBytesWriter.Close(); err != nil {
		panic(err)
	}
	return compressedBytes
}

func hashObject(objectType string, fileBytes []byte) [20]byte {
	contentString := fmt.Sprintf("%s %d\x00%s", objectType, len(fileBytes), fileBytes)

	compressedBytes := zlibCompressBytes(contentString)

	contentBytes := sha1.Sum([]byte(contentString))
	contentHexString := hex.EncodeToString(contentBytes[:])
	dir := fmt.Sprintf("%s%s", objectsDir, contentHexString[:2])
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		panic(err)
	}

	err := os.WriteFile(fmt.Sprintf("%s/%s", dir, contentHexString[2:]), compressedBytes.Bytes(), os.ModePerm)
	if err != nil {
		panic(err)
	}
	// fmt.Printf("%s", contentHexString)
	return contentBytes
}

func lsTree(inputSha string) {
	decompressedBytes := gitObjReaderHelper(inputSha)

	nullByteIdx := bytes.IndexByte(decompressedBytes, 0)

	// headerBytes := decompressedBytes[:nullByteIdx+1]
	contentBytes := decompressedBytes[nullByteIdx+1:]

	parts := bytes.Split(contentBytes, []byte(nullOperator))
	newParts := parts[:len(parts)-1] //removing last sha
	// for _, part := range newParts {
	// 	fmt.Println(string(part))
	// }
	for i := range newParts {
		splittedEntry := bytes.Split(newParts[i], []byte(" "))
		// fmt.Printf("splittedEntry: %v\n", splittedEntry)
		// for j := range splittedEntry {
		// 	fmt.Printf("splittedEntry[%d]: %s\n", j, string(splittedEntry[j]))
		// }
		// this will ensure to get the name of the file/dir even when there are spaces in the sha bytes
		fmt.Println(string(splittedEntry[len(splittedEntry)-1]))
	}
	os.Exit(0)
}

// iterative postorder traversal using the two-stack method
func (t *Tree) PostOrderTraversal() []*Node {
	if t.Root == nil {
		return nil
	}

	// Stack 1 (S1): Used for the modified Preorder traversal.
	stack1 := []*Node{t.Root}

	// Result slice: Stores the nodes in reverse postorder (Root -> Children_k -> ... -> Children_1).
	var result []*Node

	for len(stack1) > 0 {
		// 1. Pop the node from S1 (Peek and Pop combined)
		n := stack1[len(stack1)-1]
		stack1 = stack1[:len(stack1)-1]

		// 2. Add the node's value to the result slice (acting as Stack 2)
		result = append(result, n)

		// 3. Push all children onto S1 from LEFT-TO-RIGHT.
		// Since the stack is LIFO, this ensures they are processed Right-to-Left later.
		for _, child := range n.Children {
			if child != nil {
				stack1 = append(stack1, child)
			}
		}
	}

	// 4. Reverse the result slice to get the correct Postorder sequence
	// (Children_1 -> ... -> Children_k -> Root).
	slices.Reverse(result)

	return result
}

func NewTreeEntry(node *Node) *TreeEntry {
	objectBytes := new(bytes.Buffer)
	objectBytes.WriteString(node.Info.FileMode)
	objectBytes.WriteByte(' ')
	objectBytes.WriteString(node.Name)
	objectBytes.WriteByte(0)
	objectBytes.Write(node.Info.HashBytes[:])
	return &TreeEntry{Name: node.Name, Buffer: objectBytes}
}

func concatenateTreeEntries(treeEntries []*TreeEntry) ([]byte, error) {
	// 1. Calculate the total size required for the destination slice
	totalSize := 0
	for _, entry := range treeEntries {
		totalSize += entry.Buffer.Len()
	}

	// 2. Pre-allocate the final byte slice with the exact total size
	finalBytes := make([]byte, totalSize)

	// 3. Copy data from all entry buffers into the final slice
	offset := 0
	for _, entry := range treeEntries {

		// Get the underlying byte slice from the entry's buffer
		srcData := entry.Buffer.Bytes()
		dataLen := len(srcData)

		// Copy the source data into the correct location in the destination slice
		n := copy(finalBytes[offset:offset+dataLen], srcData)

		// This error check is mostly theoretical for simple memory copies
		if n != dataLen {
			// This would only happen if the copy was unexpectedly incomplete
			return nil, fmt.Errorf("failed to copy all bytes for entry: %s", entry.Name)
		}

		// Advance the offset for the next copy operation
		offset += n
	}

	// 4. Return the complete, pre-allocated slice
	return finalBytes, nil
}

func writeTree(sourceDir string) {
	// Get absolute path for source directory
	absSourceDir, err := filepath.Abs(sourceDir)
	if err != nil {
		log.Fatalf("failed to get absolute path: %s", err)
	}

	tree := NewTree(absSourceDir)
	err = filepath.WalkDir(absSourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.Name() == ".git" {
			return filepath.SkipDir
		}

		fileInfo, err := d.Info()
		if err != nil {
			return fmt.Errorf("failed to get file info for %s: %w", path, err)
		}

		gitObjectMode, err := getGitObjectMode(d)
		if err != nil {
			return fmt.Errorf("failed to get git object mode for %s: %w", path, err)
		}

		_, err = tree.Insert(absSourceDir, path, d.IsDir(), int(fileInfo.Size()), gitObjectMode)
		if err != nil {
			return fmt.Errorf("failed to insert %s into tree: %w", path, err)
		}

		return nil
	})
	if err != nil {
		log.Fatalf("failed to walk directory: %s", err)
	}

	listOfNodes := tree.PostOrderTraversal()

	i := 0
	for i < len(listOfNodes) {
		currentNode := listOfNodes[i]
		parentNode := currentNode.Parent

		if parentNode == nil {
			break
		}

		// Collect all siblings of the current node
		var treeEntries []*TreeEntry
		j := i
		for j < len(listOfNodes) && listOfNodes[j].Parent == parentNode {
			node := listOfNodes[j]

			// Ensure blobs are created for all files
			if !node.Info.IsDir && node.Info.HashBytes == [20]byte{} {
				node.Info.HashBytes = createBlobObjectFromFilePath(node.Info.FullPath)
			}

			treeEntries = append(treeEntries, NewTreeEntry(node))
			j++
		}

		// Sort tree entries by name as required by Git
		sort.Slice(treeEntries, func(i, j int) bool {
			return treeEntries[i].Name < treeEntries[j].Name
		})

		// Create tree object for the parent directory
		contentBytes, err := concatenateTreeEntries(treeEntries)
		if err != nil {
			log.Fatalf("failed to concatenate tree entries: %s", err)
		}

		parentNode.Info.HashBytes = hashObject("tree", contentBytes)
		i = j
	}

	// Process root node last
	if len(listOfNodes) > 0 {
		rootNode := tree.Root
		var rootEntries []*TreeEntry

		// Sort children by name
		rootEntries = make([]*TreeEntry, 0, len(rootNode.Children))
		for _, child := range rootNode.Children {
			rootEntries = append(rootEntries, NewTreeEntry(child))
		}
		sort.Slice(rootEntries, func(i, j int) bool {
			return rootEntries[i].Name < rootEntries[j].Name
		})

		if len(rootEntries) > 0 {
			contentBytes, err := concatenateTreeEntries(rootEntries)
			if err != nil {
				log.Fatalf("failed to concatenate root tree entries: %s", err)
			}

			tree.Root.Info.HashBytes = hashObject("tree", contentBytes)
		}
	}

	fmt.Fprintf(os.Stdout, "%s", hex.EncodeToString(tree.Root.Info.HashBytes[:]))
}

func commitTree(treeSha string, parentCommitSha string, commitMessage string) [20]byte {
	objectBytes := new(bytes.Buffer)
	fmt.Fprintf(objectBytes, "tree %s\n", treeSha)
	fmt.Fprintf(objectBytes, "parent %s\n", parentCommitSha)
	t := time.Now()
	fmt.Fprintf(objectBytes, "author %s %d %s\n", authorString, t.Unix(), t.Format("-0700"))
	fmt.Fprintf(objectBytes, "committer %s %d %s\n", committerString, t.Unix(), t.Format("-0700"))
	fmt.Fprintf(objectBytes, "\n%s\n", commitMessage)

	return hashObject("commit", objectBytes.Bytes())
}

// Usage: your_program.sh <command> <arg1> <arg2> ...
func main() {
	// You can use print statements as follows for debugging, they'll be visible when running tests.
	fmt.Fprintf(os.Stderr, "Logs from your program will appear here!\n")
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: mygit <command> [<args>...]\n")
		os.Exit(1)
	}

	switch command := os.Args[1]; command {
	case "init":
		// TODO: Uncomment the code below to pass the first stage!

		for _, dir := range []string{".git", ".git/objects", ".git/refs"} {
			if err := os.MkdirAll(dir, 0755); err != nil {
				fmt.Fprintf(os.Stderr, "Error creating directory: %s\n", err)
			}
		}

		headFileContents := []byte("ref: refs/heads/main\n")
		if err := os.WriteFile(".git/HEAD", headFileContents, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing file: %s\n", err)
		}

		fmt.Println("Initialized git directory")

	case "cat-file":
		if len(os.Args) < 4 {
			fmt.Fprintf(os.Stderr, "usage: mygit cat-file -p [<args>...]\n")
			os.Exit(1)
		}

		inputSha := os.Args[3]
		catFile(inputSha)

	case "hash-object":
		if len(os.Args) < 4 {
			fmt.Fprintf(os.Stderr, "usage: mygit hash-object -w [<args>...]\n")
			os.Exit(1)
		}

		hashBytes := createBlobObjectFromFilePath(os.Args[3])
		fmt.Println(hex.EncodeToString(hashBytes[:]))
		os.Exit(0)
	case "ls-tree":
		lsTree(os.Args[3])

	case "write-tree":
		writeTree(".")
		os.Exit(0)
	case "commit-tree":
		// os.Args[2] == "<tree_sha>"
		// os.Args[4] == "<parent_commit_sha>"
		// os.Args[6] == "<commit_message>"
		hashBytes := commitTree(os.Args[2], os.Args[4], os.Args[6])
		fmt.Println(hex.EncodeToString(hashBytes[:]))
		os.Exit(0)
	}
}
