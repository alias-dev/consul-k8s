package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	tocPrefix = "## Top-Level Stanzas\n\nUse these links to navigate to a particular top-level stanza.\n\n"
	tocSuffix = "\n## All Values"
)

func main() {
	validateFlag := flag.Bool("validate", false, "only validate that the markdown can be generated, don't actually generate anything")
	templateFlag := flag.String("template", "table", "template to use for generating the markdown")
	consulRepoPath := "../../../consul"
	flag.Parse()

	if len(os.Args) > 5 {
		fmt.Println("Error: extra arguments")
		os.Exit(1)
	}

	if !*validateFlag {
		// Only argument is path to Consul repo. If not set then we default.
		if len(os.Args) < 2 {
			abs, _ := filepath.Abs(consulRepoPath)
			fmt.Printf("Defaulting to Consul repo path: %s\n", abs)
		} else {
			// Support absolute and relative paths to the Consul repo.
			if filepath.IsAbs(os.Args[1]) {
				consulRepoPath = os.Args[1]
			} else {
				consulRepoPath = filepath.Join("../..", os.Args[1])
			}
			abs, _ := filepath.Abs(consulRepoPath)
			fmt.Printf("Using Consul repo path: %s\n", abs)
		}
	}

	// Parse the values.yaml file.
	inputBytes, err := ioutil.ReadFile("../../charts/consul/values.yaml")
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	out, err := GenerateDocs(string(inputBytes), *templateFlag)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	// If we're just validating that generation will succeed then we're done.
	if *validateFlag {
		// fmt.Println("Validation successful")
		fmt.Println(out)
		os.Exit(0)
	}

	// Otherwise we'll go on to write the changes to the helm docs.
	helmReferenceFile := filepath.Join(consulRepoPath, "website/content/docs/k8s/helm.mdx")
	helmReferenceBytes, err := ioutil.ReadFile(helmReferenceFile)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
	helmReferenceContents := string(helmReferenceBytes)

	// Swap out the contents between the codegen markers.
	startStr := "<!-- codegen: start -->\n\n"
	endStr := "\n  <!-- codegen: end -->"
	start := strings.Index(helmReferenceContents, startStr)
	if start == -1 {
		fmt.Printf("%q not found in %q\n", startStr, helmReferenceFile)
		os.Exit(1)
	}
	end := strings.Index(helmReferenceContents, endStr)
	if end == -1 {
		fmt.Printf("%q not found in %q\n", endStr, helmReferenceFile)
		os.Exit(1)
	}

	newMdx := helmReferenceContents[0:start+len(startStr)] + out + helmReferenceContents[end:]
	err = ioutil.WriteFile(helmReferenceFile, []byte(newMdx), 0644)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
	abs, _ := filepath.Abs(helmReferenceFile)
	fmt.Printf("Updated with generated docs: %s\n", abs)
}

func GenerateDocs(yamlStr, templateName string) (string, error) {
	node, err := Parse(yamlStr)
	if err != nil {
		return "", err
	}

	if templateName == "table" {
		return FormatAsTables(node)
	} else if templateName == "list" {
		return FormatAsList(node)
	} else {
		return "", fmt.Errorf("unknown template name: %q", templateName)
	}
}

// allScalars returns true if content contains only scalar nodes
// with no chidren.
func allScalars(content []*yaml.Node) bool {
	for _, n := range content {
		if n.Kind != yaml.ScalarNode || len(n.Content) > 0 {
			return false
		}
	}
	return true
}

func buildDocNode(nodeContentIdx int, currNode *yaml.Node, nodeContent []*yaml.Node, parentBreadcrumb string, parentWasMap bool) (DocNode, error) {
	// Check for the @recurse: false annotation.
	// In this case we construct our node and then don't recurse further.
	if match := recurseAnnotation.FindStringSubmatch(currNode.HeadComment); len(match) > 0 && match[1] == "false" {
		return DocNode{
			Column:           currNode.Column,
			ParentBreadcrumb: parentBreadcrumb,
			ParentWasMap:     false,
			Key:              currNode.Value,
			Comment:          currNode.HeadComment,
		}, nil
	}

	// Nodes should come in pairs.
	if len(nodeContent) < nodeContentIdx+1 {
		return DocNode{}, &ParseError{
			ParentAnchor: parentBreadcrumb,
			CurrAnchor:   currNode.Value,
			Err:          fmt.Sprintf("content length incorrect, expected %d got %d", nodeContentIdx+1, len(nodeContent)),
		}
	}

	next := nodeContent[nodeContentIdx+1]

	switch next.Kind {

	// If it's a scalar then this is a simple key: value node.
	case yaml.ScalarNode:
		return DocNode{
			ParentBreadcrumb: parentBreadcrumb,
			ParentWasMap:     parentWasMap,
			Column:           currNode.Column,
			Key:              currNode.Value,
			Comment:          currNode.HeadComment,
			KindTag:          next.Tag,
			Default:          next.Value,
		}, nil

	// If it's a map then we will need to recurse into it.
	case yaml.MappingNode:
		docNode := DocNode{
			ParentBreadcrumb: parentBreadcrumb,
			ParentWasMap:     parentWasMap,
			Column:           currNode.Column,
			Key:              currNode.Value,
			Comment:          currNode.HeadComment,
			KindTag:          next.Tag,
		}
		var err error
		docNode.Children, err = parseNodeContent(next.Content, docNode.HTMLAnchor(), false)
		if err != nil {
			return DocNode{}, err
		}
		return docNode, nil

	// If it's a sequence, i.e. array, then we have to handle it differently
	// depending on its contents.
	case yaml.SequenceNode:
		// If it's empty then its just a key with a default of empty array.
		if len(next.Content) == 0 {
			return DocNode{
				ParentBreadcrumb: parentBreadcrumb,
				ParentWasMap:     parentWasMap,
				Column:           currNode.Column,
				Key:              currNode.Value,
				// Default is empty array.
				Default: "[]",
				Comment: currNode.HeadComment,
				KindTag: next.Tag,
			}, nil

			// If it's full of scalars, e.g. key: [a, b] then we can stop recursing
			// and use the value as the default.
		} else if allScalars(next.Content) {
			inlineYaml, err := toInlineYaml(next.Content)
			if err != nil {
				return DocNode{}, &ParseError{
					ParentAnchor: parentBreadcrumb,
					CurrAnchor:   currNode.Value,
					Err:          err.Error(),
				}
			}
			return DocNode{
				ParentBreadcrumb: parentBreadcrumb,
				ParentWasMap:     parentWasMap,
				Column:           currNode.Column,
				Key:              currNode.Value,
				// Default will be the yaml value.
				Default: inlineYaml,
				Comment: currNode.HeadComment,
				KindTag: next.Tag,
			}, nil
		} else {

			// Otherwise we need to recurse into each element of the array.
			docNode := DocNode{
				ParentBreadcrumb: parentBreadcrumb,
				ParentWasMap:     parentWasMap,
				Column:           currNode.Column,
				Key:              currNode.Value,
				Comment:          currNode.HeadComment,
				KindTag:          next.Tag,
			}
			var err error
			docNode.Children, err = parseNodeContent(next.Content, docNode.HTMLAnchor(), false)
			if err != nil {
				return DocNode{}, err
			}
			return docNode, nil
		}
	}
	return DocNode{}, fmt.Errorf("fell through cases unexpectedly at breadcrumb: %s", parentBreadcrumb)
}

func generateTOC(node DocNode) string {
	toc := tocPrefix

	for _, c := range node.Children {
		toc += fmt.Sprintf("- [`%s`](#%s)\n", c.Key, strings.ToLower(c.Key))
	}

	return toc + tocSuffix
}
