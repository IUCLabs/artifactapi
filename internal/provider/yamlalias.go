package provider

import (
	"errors"

	"gopkg.in/yaml.v3"
)

const MaxAliasExpandedNodes = 100000

// ExpandYAMLAliases inlines alias targets and drops anchors so a node map
// stays valid when copied away from the document that held its anchors;
// budget caps nodes reached through aliases against alias bombs.
func ExpandYAMLAliases(m map[string]yaml.Node, budget *int) (map[string]yaml.Node, error) {
	var expand func(n *yaml.Node, viaAlias bool) (*yaml.Node, error)
	expand = func(n *yaml.Node, viaAlias bool) (*yaml.Node, error) {
		if viaAlias {
			*budget--
			if *budget < 0 {
				return nil, errors.New("yaml aliases expand to too many nodes")
			}
		}
		if n.Kind == yaml.AliasNode {
			return expand(n.Alias, true)
		}
		c := *n
		c.Anchor = ""
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, ch := range n.Content {
			var err error
			if c.Content[i], err = expand(ch, viaAlias); err != nil {
				return nil, err
			}
		}
		return &c, nil
	}
	out := make(map[string]yaml.Node, len(m))
	for k, v := range m {
		e, err := expand(&v, false)
		if err != nil {
			return nil, err
		}
		out[k] = *e
	}
	return out, nil
}
