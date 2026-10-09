// Command schema-census checks required fields against generated CRD schemas.
// It is an offline preflight, not a substitute for live API admission or CEL.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/util/yaml"
)

type report struct {
	File    string   `json:"file"`
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Missing []string `json:"missingRequiredFields"`
}

func object(value any) map[string]any { valueMap, _ := value.(map[string]any); return valueMap }
func readDocuments(path string) ([]map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	reader := yaml.NewYAMLReader(bufio.NewReader(file))
	var documents []map[string]any
	for {
		data, readErr := reader.Read()
		if readErr == io.EOF {
			return documents, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		converted, convertErr := yaml.ToJSON(data)
		if convertErr != nil {
			return nil, convertErr
		}
		var document map[string]any
		if jsonErr := json.Unmarshal(converted, &document); jsonErr != nil {
			return nil, jsonErr
		}
		if document != nil {
			documents = append(documents, document)
		}
	}
}
func missingFields(value any, schema map[string]any, path string) []string {
	missing := []string{}
	switch actual := value.(type) {
	case map[string]any:
		required, _ := schema["required"].([]any)
		for _, name := range required {
			key, _ := name.(string)
			if _, present := actual[key]; !present {
				missing = append(missing, path+"."+key)
			}
		}
		properties := object(schema["properties"])
		for key, child := range actual {
			childSchema := object(properties[key])
			if childSchema == nil {
				childSchema = object(schema["additionalProperties"])
			}
			missing = append(missing, missingFields(child, childSchema, path+"."+key)...)
		}
	case []any:
		for i, child := range actual {
			missing = append(missing, missingFields(child, object(schema["items"]), fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return missing
}
func run() error {
	files, err := filepath.Glob("config/crd/bases/*.yaml")
	if err != nil {
		return err
	}
	schemas := map[string]map[string]any{}
	for _, file := range files {
		docs, readErr := readDocuments(file)
		if readErr != nil {
			return readErr
		}
		for _, doc := range docs {
			spec := object(doc["spec"])
			names := object(spec["names"])
			kind, _ := names["kind"].(string)
			versions, _ := spec["versions"].([]any)
			for _, version := range versions {
				schema := object(object(version)["schema"])
				schemas[kind] = object(schema["openAPIV3Schema"])
			}
		}
	}
	if len(schemas) == 0 {
		return fmt.Errorf("run from repository root with generated CRD schemas")
	}
	var reports []report
	failed := false
	for _, file := range os.Args[1:] {
		docs, readErr := readDocuments(file)
		if readErr != nil {
			return readErr
		}
		for _, doc := range docs {
			kind, _ := doc["kind"].(string)
			if schema, found := schemas[kind]; found {
				metadata := object(doc["metadata"])
				name, _ := metadata["name"].(string)
				missing := missingFields(doc, schema, kind)
				reports = append(reports, report{File: file, Kind: kind, Name: name, Missing: missing})
				failed = failed || len(missing) > 0
			}
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(reports); err != nil {
		return err
	}
	if failed {
		return fmt.Errorf("required CRD fields are missing")
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
