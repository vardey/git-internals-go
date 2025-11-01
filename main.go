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
	"strings"
)

const objectsDir string = ".git/objects/"
const nullOperator string = "\x00"

// Info holds the metadata for a file or directory.
type Info struct {
	Size      int      // Size of the file in bytes (0 for directories)
	FileMode  int      // File permissions and mode
	HashBytes [20]byte // Content hash (20 bytes, e.g., SHA-1), initially empty
	IsDir     bool     // True if it is a directory, false if it is a file
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

// NewTree initializes a new Tree with a root directory.
func NewTree(srcDir string) *Tree {
	// The root node is always a directory and has no name in the context of the path.
	root := NewNode(srcDir, true, 0, 040000, nil)
	return &Tree{Root: root}
}

// NewNode is a constructor for creating a new Node.
func NewNode(name string, isDir bool, size int, mode int, parent *Node) *Node {
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

// Insert inserts a new file or directory into the tree based on its full path.
// It creates any necessary parent directories along the way.
func (t *Tree) Insert(fullPath string, isDir bool, size int, mode int) (*Node, error) {
	// Clean the path to handle redundancies like 'a//b' and resolve '.' and '..'
	// and trim leading/trailing separators so Split works cleanly.
	cleanedPath := filepath.Clean(fullPath)
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
				newNode = NewNode(name, isDir, size, mode, currentNode)

				// New logic: Calculate hash for files using the parent directory's path
				if !isDir {
					fileBytes, err := os.ReadFile(os.Args[3])
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error reading file: %s\n", err)
						return nil, err
					}
					// Use filepath.Dir to get the parent directory path from the cleaned path
					newNode.Info.HashBytes = hashObject("blob", fileBytes)
				}
			} else {
				// Intermediate node must be a directory
				newNode = NewNode(name, true, 0, mode, currentNode)
			}

			currentNode.Children[name] = newNode
			currentNode = newNode
		}
	}

	return currentNode, nil
}

// getGitMode determines the 6-digit Git mode string for a given file entry.
func getGitObjectMode(d fs.DirEntry) (int, error) {
	// 1. Check for Directory
	if d.IsDir() {
		return 040000, nil // Git mode for a tree object (directory)
	}

	// 2. Check for Symbolic Link
	if d.Type()&fs.ModeSymlink != 0 {
		return 120000, nil // Git mode for a symbolic link
	}

	// 3. Handle Regular Files (Blob Objects)
	if d.Type().IsRegular() {
		// Need full os.FileInfo to check for the executable bit
		info, err := d.Info()
		if err != nil {
			return 0, err
		}

		// Check the executable bit:
		// 0111 (octal) represents the executable bits (user, group, other)
		// If ANY of the executable bits are set, Git treats it as an executable file.
		if info.Mode()&0111 != 0 {
			return 100755, nil // Executable file
		}

		return 100644, nil // Non-executable file
	}

	// For any other special file types (like devices, sockets, etc.), Git ignores them
	return 0, fmt.Errorf("unsupported file type for Git: %s", d.Type().String())
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
	fmt.Printf("%s", contentHexString)
	return contentBytes
}

func lsTree(inputSha string) {
	decompressedBytes := gitObjReaderHelper(inputSha)

	nullByteIdx := bytes.IndexByte(decompressedBytes, 0)

	// headerBytes := decompressedBytes[:nullByteIdx+1]
	contentBytes := decompressedBytes[nullByteIdx+1:]

	parts := bytes.Split(contentBytes, []byte(nullOperator))
	newParts := parts[:len(parts)-1] //removing last sha

	for i := range newParts {
		splittedEntry := bytes.Split(newParts[i], []byte(" "))
		fmt.Println(string(splittedEntry[1]))
	}
	os.Exit(0)
}

/*
header\0blobObject(src.txt)\0treeObject(subsrc)

treeObject(subsrc) =
*/
func writeTree() {
	srcDir := "./app" //objectsDir
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {

		if d.Name() == ".git" {
			return filepath.SkipDir

		}
		fileInfo, errr := d.Info()
		if errr != nil {
			fmt.Println(errr)
			return errr
		}
		gitObjectMode, errr := getGitObjectMode(d)
		if errr != nil {
			fmt.Println(errr)
			return errr
		}
		fmt.Println("path: ", path, " name: ", d.Name(), "directory?", d.IsDir(), "gitObjMode: ", gitObjectMode, "file size: ", fileInfo.Size(), "+++++++")
		return nil
	})
	if err != nil {
		log.Fatalf("impossible to walk directories: %s", err)
	}
}

// Usage: your_program.sh <command> <arg1> <arg2> ...
func main() {
	// You can use print statements as follows for debugging, they'll be visible when running tests.
	fmt.Fprintf(os.Stderr, "Logs from your program will appear here!\n")

	writeTree()

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

		fileBytes, err := os.ReadFile(os.Args[3])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading file: %s\n", err)
			os.Exit(1)
		}
		hashObject("blob", fileBytes)
		os.Exit(0)
	case "ls-tree":
		lsTree(os.Args[3])

	case "write-tree":
		/*
			create a Tree with "." dir,
			Traverse the root in such a way that:
			from the leaf nodes to the top root node follow:
			at each depth create a new string array "entries"
			for leaf nodes:
			if file-> entries.append("node.mode node.name\x00node.hashBytes")
			else
			contents=("tree 0\x00") in bytes array
			node.hashBytes = hashObject("tree", contents)
			entries.append("node.mode node.name\x00node.hashBytes")

			for parent node:
			sort the entries array on the basis of node.name in each string
			entriesString = string(entries)
			contents = ("tree %d\x00%s", len(entriesString), entryString)
			hashBytes:=hashObject("tree", contents)
			since it is a new depth this we need to create a new entries array and append("mode name\x00hashBytes")
		*/
		writeTree()

	}
}
