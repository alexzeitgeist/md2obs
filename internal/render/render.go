// Package render turns raw source bytes into the exact bytes written to a
// vault materialization.
package render

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/alexzeitgeist/md2obs/internal/config"
	"gopkg.in/yaml.v3"
)

const (
	// SourceProfile records a byte-identical raw source copy.
	SourceProfile = "source-v1"
	// ProvenanceProfile records the first managed provenance-frontmatter
	// serialization format.
	ProvenanceProfile = "provenance-frontmatter-v1"
)

var bom = []byte{0xef, 0xbb, 0xbf}

// Input contains all facts needed to deterministically render a vault copy.
type Input struct {
	SourceContent  []byte
	CanonicalPath  string
	SnapshotTime   string
	WithProvenance bool
}

// Output is the rendered content together with its exact hash and the
// compatibility profile that produced it.
type Output struct {
	Content []byte
	SHA256  string
	Profile string
}

// Profile returns the renderer profile selected by configuration alone.
func Profile(cfg *config.Config) string {
	if cfg.ProvenanceFrontmatter {
		return ProvenanceProfile
	}
	return SourceProfile
}

// Render produces the exact bytes to materialize.
func Render(in Input) (Output, error) {
	profile := SourceProfile
	content := in.SourceContent
	if in.WithProvenance {
		profile = ProvenanceProfile
		var err error
		content, err = renderProvenance(in)
		if err != nil {
			return Output{}, err
		}
	}
	sum := sha256.Sum256(content)
	return Output{
		Content: content,
		SHA256:  fmt.Sprintf("%x", sum[:]),
		Profile: profile,
	}, nil
}

type frontmatter struct {
	yamlContent []byte
	body        []byte
	lineEnding  []byte
	bodyJoined  bool
}

func renderProvenance(in Input) ([]byte, error) {
	sourceContent := in.SourceContent
	hasBOM := bytes.HasPrefix(sourceContent, bom)
	afterBOM := sourceContent
	if hasBOM {
		afterBOM = afterBOM[len(bom):]
	}

	fm, recognized := recognizeFrontmatter(afterBOM)
	var mapping *yaml.Node
	if recognized {
		var err error
		mapping, err = parseMapping(fm.yamlContent)
		if err != nil {
			recognized = false
		}
	}
	if !recognized {
		fm = frontmatter{
			body:        afterBOM,
			lineEnding:  firstLineEnding(afterBOM),
			bodyJoined:  true,
			yamlContent: nil,
		}
		mapping = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}

	removeReserved(mapping)
	appendScalarPair(mapping, "md2obs_source_path", in.CanonicalPath, "!!str", yaml.DoubleQuotedStyle)
	appendScalarPair(mapping, "md2obs_imported_at", in.SnapshotTime, "!!timestamp", 0)

	encoded, err := encodeMapping(mapping, fm.lineEnding)
	if err != nil {
		return nil, fmt.Errorf("encode provenance frontmatter: %w", err)
	}

	var out bytes.Buffer
	if hasBOM {
		out.Write(bom)
	}
	out.WriteString("---")
	out.Write(fm.lineEnding)
	out.Write(encoded)
	out.WriteString("---")
	if fm.bodyJoined {
		out.Write(fm.lineEnding)
	}
	out.Write(fm.body)
	return out.Bytes(), nil
}

func recognizeFrontmatter(content []byte) (frontmatter, bool) {
	first, next, ending := logicalLine(content, 0)
	if !bytes.Equal(first, []byte("---")) || len(ending) == 0 {
		return frontmatter{}, false
	}
	for offset := next; offset <= len(content); {
		lineStart := offset
		line, after, closeEnding := logicalLine(content, offset)
		if bytes.Equal(line, []byte("---")) {
			return frontmatter{
				yamlContent: content[next:lineStart],
				body:        content[after:],
				lineEnding:  ending,
				bodyJoined:  len(closeEnding) != 0,
			}, true
		}
		if after == offset || after == len(content) && len(closeEnding) == 0 {
			break
		}
		offset = after
	}
	return frontmatter{}, false
}

// logicalLine returns a line without its delimiter, the next offset, and the
// delimiter. A final unterminated line has a nil delimiter.
func logicalLine(content []byte, offset int) (line []byte, next int, ending []byte) {
	if offset >= len(content) {
		return content[offset:], offset, nil
	}
	rel := bytes.IndexByte(content[offset:], '\n')
	if rel < 0 {
		return content[offset:], len(content), nil
	}
	end := offset + rel
	if end > offset && content[end-1] == '\r' {
		return content[offset : end-1], end + 1, []byte("\r\n")
	}
	return content[offset:end], end + 1, []byte("\n")
}

func firstLineEnding(content []byte) []byte {
	_, _, ending := logicalLine(content, 0)
	if len(ending) == 0 {
		return []byte("\n")
	}
	return ending
}

func parseMapping(content []byte) (*yaml.Node, error) {
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	err := decoder.Decode(&document)
	if err == io.EOF {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	if err != nil {
		return nil, err
	}
	node := &document
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
		}
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("frontmatter is not a mapping")
	}
	return node, nil
}

func removeReserved(mapping *yaml.Node) {
	out := mapping.Content[:0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		if key.Kind == yaml.ScalarNode &&
			(key.Value == "md2obs_source_path" || key.Value == "md2obs_imported_at") {
			continue
		}
		out = append(out, key, value)
	}
	mapping.Content = out
}

func appendScalarPair(mapping *yaml.Node, key, value, tag string, style yaml.Style) {
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value, Style: style},
	)
}

func encodeMapping(mapping *yaml.Node, lineEnding []byte) ([]byte, error) {
	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(mapping); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(encoded.Bytes(), []byte("\n"), lineEnding), nil
}
