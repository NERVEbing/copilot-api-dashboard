package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

func LoadAccountAliases(file string) (map[string]string, error) {
	if file == "" {
		return nil, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, errors.New("cannot read account aliases file")
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, errors.New("cannot read account aliases file")
	}
	if len(data) > 1<<20 {
		return nil, errors.New("account aliases YAML exceeds size limit")
	}
	var doc struct {
		Aliases yaml.Node `yaml:"aliases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return nil, errors.New("invalid account aliases YAML")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("account aliases YAML must contain one document")
	}
	if doc.Aliases.Kind != yaml.MappingNode {
		return nil, errors.New("account aliases YAML requires an aliases mapping")
	}
	aliases := make(map[string]string, len(doc.Aliases.Content)/2)
	for i := 0; i < len(doc.Aliases.Content); i += 2 {
		key, value := doc.Aliases.Content[i], doc.Aliases.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "" || strings.TrimSpace(key.Value) != key.Value || strings.ContainsAny(key.Value, "\r\n\t") {
			return nil, fmt.Errorf("invalid account aliases login at line %d", key.Line)
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || strings.TrimSpace(value.Value) == "" {
			return nil, fmt.Errorf("account alias at line %d must be a non-empty string", value.Line)
		}
		login := strings.ToLower(key.Value)
		if _, exists := aliases[login]; exists {
			return nil, fmt.Errorf("duplicate account aliases login at line %d", key.Line)
		}
		aliases[login] = value.Value
	}
	return aliases, nil
}
