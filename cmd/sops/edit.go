package main

import (
	"fmt"
	"io/ioutil"
	"os"

	"crypto/md5"
	"io"
	"os/exec"
	"strings"

	"path"

	"bufio"
	"bytes"

	"github.com/google/shlex"
	"go.mozilla.org/sops"
	"go.mozilla.org/sops/keyservice"
	"go.mozilla.org/sops/stores/json"
	"gopkg.in/urfave/cli.v1"
)

type EditOpts struct {
	Cipher         sops.DataKeyCipher
	InputStore     sops.Store
	OutputStore    sops.Store
	InputPath      string
	IgnoreMAC      bool
	KeyServices    []keyservice.KeyServiceClient
	ShowMasterKeys bool
}

type EditExampleOpts struct {
	EditOpts
	UnencryptedSuffix string
	KeyGroups         []sops.KeyGroup
	GroupQuorum       uint
}

var exampleTree = sops.TreeBranch{
	sops.TreeItem{
		Key:   "hello",
		Value: `Welcome to SOPS! Edit this file as you please!`,
	},
	sops.TreeItem{
		Key:   "example_key",
		Value: "example_value",
	},
	sops.TreeItem{
		Key: "example_array",
		Value: []interface{}{
			"example_value1",
			"example_value2",
		},
	},
	sops.TreeItem{
		Key:   "example_number",
		Value: 1234.56789,
	},
	sops.TreeItem{
		Key:   "example_booleans",
		Value: []interface{}{true, false},
	},
}

type runEditorUntilOkOpts struct {
	TmpFile        *os.File
	OriginalHash   []byte
	InputStore     sops.Store
	ShowMasterKeys bool
	Tree           *sops.Tree
}

func EditExample(opts EditExampleOpts) ([]byte, error) {
	// Load the example file
	var fileBytes []byte
	if _, ok := opts.InputStore.(*json.BinaryStore); ok {
		// Get the value under the first key
		fileBytes = []byte(exampleTree[0].Value.(string))
	} else {
		var err error
		fileBytes, err = opts.InputStore.Marshal(exampleTree)
		if err != nil {
			return nil, err
		}
	}
	var tree sops.Tree
	branch, err := opts.InputStore.Unmarshal(fileBytes)
	if err != nil {
		return nil, cli.NewExitError(fmt.Sprintf("Error unmarshalling file: %s", err), exitCouldNotReadInputFile)
	}
	tree.Branch = branch
	tree.Metadata = sops.Metadata{
		KeyGroups:         opts.KeyGroups,
		UnencryptedSuffix: opts.UnencryptedSuffix,
		Version:           version,
		ShamirQuorum:      int(opts.GroupQuorum),
	}

	// Generate a data key
	dataKey, errs := tree.GenerateDataKeyWithKeyServices(opts.KeyServices)
	if len(errs) > 0 {
		return nil, cli.NewExitError(fmt.Sprintf("Error encrypting the data key with one or more master keys: %s", errs), exitCouldNotRetrieveKey)
	}
	stash := make(map[string][]interface{})

	return edit(opts.EditOpts, &tree, dataKey, stash)
}

func Edit(opts EditOpts) ([]byte, error) {
	// Load the file
	tree, err := loadEncryptedFile(opts.InputStore, opts.InputPath)
	if err != nil {
		return nil, err
	}
	// Decrypt the file
	stash := make(map[string][]interface{})
	dataKey, err := decryptTree(decryptTreeOpts{
		Stash: stash, Cipher: opts.Cipher, IgnoreMac: opts.IgnoreMAC, Tree: tree, KeyServices: opts.KeyServices,
	})
	if err != nil {
		return nil, err
	}

	return edit(opts, tree, dataKey, stash)
}

func edit(opts EditOpts, tree *sops.Tree, dataKey []byte, stash map[string][]interface{}) ([]byte, error) {
	// Create temporary file for editing
	tmpdir, err := ioutil.TempDir("", "")
	if err != nil {
		return nil, cli.NewExitError(fmt.Sprintf("Could not create temporary directory: %s", err), exitCouldNotWriteOutputFile)
	}
	defer os.RemoveAll(tmpdir)
	tmpfile, err := os.Create(path.Join(tmpdir, path.Base(opts.InputPath)))
	if err != nil {
		return nil, cli.NewExitError(fmt.Sprintf("Could not create temporary file: %s", err), exitCouldNotWriteOutputFile)
	}

	// Write to temporary file
	var out []byte
	if opts.ShowMasterKeys {
		out, err = opts.OutputStore.MarshalWithMetadata(tree.Branch, tree.Metadata)
	} else {
		out, err = opts.OutputStore.Marshal(tree.Branch)
	}
	if err != nil {
		return nil, cli.NewExitError(fmt.Sprintf("Could not marshal tree: %s", err), exitErrorDumpingTree)
	}
	_, err = tmpfile.Write(out)
	if err != nil {
		return nil, cli.NewExitError(fmt.Sprintf("Could not write output file: %s", err), exitCouldNotWriteOutputFile)
	}

	// Compute file hash to detect if the file has been edited
	origHash, err := hashFile(tmpfile.Name())
	if err != nil {
		return nil, cli.NewExitError(fmt.Sprintf("Could not hash file: %s", err), exitCouldNotReadInputFile)
	}

	// Let the user edit the file
	runEditorUntilOk(runEditorUntilOkOpts{
		InputStore: opts.InputStore, OriginalHash: origHash, TmpFile: tmpfile,
		ShowMasterKeys: opts.ShowMasterKeys, Tree: tree})

	// Encrypt the file
	err = encryptTree(encryptTreeOpts{
		Stash: stash, DataKey: dataKey, Tree: tree, Cipher: opts.Cipher,
	})
	if err != nil {
		return nil, err
	}

	// Output the file
	encryptedFile, err := opts.OutputStore.MarshalWithMetadata(tree.Branch, tree.Metadata)
	if err != nil {
		return nil, cli.NewExitError(fmt.Sprintf("Could not marshal tree: %s", err), exitErrorDumpingTree)
	}
	return encryptedFile, nil
}

func runEditorUntilOk(opts runEditorUntilOkOpts) error {
	for {
		err := runEditor(opts.TmpFile.Name())
		if err != nil {
			return cli.NewExitError(fmt.Sprintf("Could not run editor: %s", err), exitNoEditorFound)
		}
		newHash, err := hashFile(opts.TmpFile.Name())
		if err != nil {
			return cli.NewExitError(fmt.Sprintf("Could not hash file: %s", err), exitCouldNotReadInputFile)
		}
		if bytes.Equal(newHash, opts.OriginalHash) {
			return cli.NewExitError("File has not changed, exiting.", exitFileHasNotBeenModified)
		}
		edited, err := ioutil.ReadFile(opts.TmpFile.Name())
		if err != nil {
			return cli.NewExitError(fmt.Sprintf("Could not read edited file: %s", err), exitCouldNotReadInputFile)
		}
		newBranch, err := opts.InputStore.Unmarshal(edited)
		if err != nil {
			fmt.Printf("Could not load tree: %s\nProbably invalid syntax. Press a key to return to the editor, or Ctrl+C to exit.", err)
			bufio.NewReader(os.Stdin).ReadByte()
			continue
		}
		if opts.ShowMasterKeys {
			metadata, err := opts.InputStore.UnmarshalMetadata(edited)
			if err != nil {
				fmt.Printf("sops branch is invalid: %s.\nPress a key to return to the editor, or Ctrl+C to exit.", err)
				bufio.NewReader(os.Stdin).ReadByte()
				continue
			}
			opts.Tree.Metadata = *metadata
		}
		opts.Tree.Branch = newBranch
		needVersionUpdated, err := AIsNewerThanB(version, opts.Tree.Metadata.Version)
		if err != nil {
			return cli.NewExitError(fmt.Sprintf("Failed to compare document version %q with program version %q: %v", opts.Tree.Metadata.Version, version, err), exitFailedToCompareVersions)
		}
		if needVersionUpdated {
			opts.Tree.Metadata.Version = version
		}
		if opts.Tree.Metadata.MasterKeyCount() == 0 {
			fmt.Println("No master keys were provided, so sops can't encrypt the file.\nPress a key to return to the editor, or Ctrl+C to exit.")
			bufio.NewReader(os.Stdin).ReadByte()
			continue
		}
		break
	}
	return nil
}

func hashFile(filePath string) ([]byte, error) {
	var result []byte
	file, err := os.Open(filePath)
	if err != nil {
		return result, err
	}
	defer file.Close()
	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return result, err
	}
	return hash.Sum(result), nil
}

func runEditor(path string) error {
	editor := os.Getenv("EDITOR")
	var cmd *exec.Cmd
	if editor == "" {
		cmd = exec.Command("which", "vim", "nano")
		out, err := cmd.Output()
		if err != nil {
			panic("Could not find any editors")
		}
		cmd = exec.Command(strings.Split(string(out), "\n")[0], path)
	} else {
		parts, err := shlex.Split(editor)
		if err != nil {
			return fmt.Errorf("Invalid $EDITOR: %s", editor)
		}
		parts = append(parts, path)
		cmd = exec.Command(parts[0], parts[1:]...)
	}

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
