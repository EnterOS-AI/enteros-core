package handlers

import "gopkg.in/yaml.v3"

func yamlUnmarshalForTest(b []byte, v any) error { return yaml.Unmarshal(b, v) }
