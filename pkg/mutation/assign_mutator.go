package mutation

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/go-cmp/cmp"
	mutationsv1alpha1 "github.com/open-policy-agent/gatekeeper/apis/mutations/v1alpha1"
	"github.com/open-policy-agent/gatekeeper/pkg/mutation/path/parser"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// AssignMutator is a mutator object built out of a
// Assign instance.
type AssignMutator struct {
	id       ID
	assign   *mutationsv1alpha1.Assign
	path     *parser.Path
	bindings []SchemaBinding
}

// AssignMutator implements mutatorWithSchema
var _ MutatorWithSchema = &AssignMutator{}

func (m *AssignMutator) Matches(obj runtime.Object, ns *corev1.Namespace) bool {
	matches, err := Matches(m.assign.Spec.Match, obj, ns)
	if err != nil {
		log.Error(err, "AssignMutator.Matches failed", "assign", m.assign.Name)
		return false
	}
	return matches
}

func (m *AssignMutator) Mutate(obj *unstructured.Unstructured) error {
	return Mutate(m, obj)
}
func (m *AssignMutator) ID() ID {
	return m.id
}

func (m *AssignMutator) SchemaBindings() []SchemaBinding {
	return m.bindings
}

func (m *AssignMutator) Value() (interface{}, error) {
	return unmarshalValue(m.assign.Spec.Parameters.Assign.Raw)
}

func (m *AssignMutator) HasDiff(mutator Mutator) bool {
	toCheck, ok := mutator.(*AssignMutator)
	if !ok { // different types, different
		return true
	}

	if !cmp.Equal(toCheck.id, m.id) {
		return true
	}
	if !cmp.Equal(toCheck.path, m.path) {
		return true
	}
	if !cmp.Equal(toCheck.bindings, m.bindings) {
		return true
	}

	// any difference in spec may be enough
	if !cmp.Equal(toCheck.assign.Spec, m.assign.Spec) {
		return true
	}

	return false
}

func (m *AssignMutator) Path() *parser.Path {
	return m.path
}

func (m *AssignMutator) DeepCopy() Mutator {
	res := &AssignMutator{
		id:     m.id,
		assign: m.assign.DeepCopy(),
		path: &parser.Path{
			Nodes: make([]parser.Node, len(m.path.Nodes)),
		},
		bindings: make([]SchemaBinding, len(m.bindings)),
	}
	copy(res.path.Nodes, m.path.Nodes)
	copy(res.bindings, m.bindings)
	return res
}

// MutatorForAssign returns an AssignMutator built from
// the given assign instance.
func MutatorForAssign(assign *mutationsv1alpha1.Assign) (*AssignMutator, error) {
	id, err := MakeID(assign)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to retrieve id for assign type")
	}

	path, err := parser.Parse(assign.Spec.Location)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to parse the location specified")
	}

	return &AssignMutator{
		id:       id,
		assign:   assign.DeepCopy(),
		bindings: applyToToBindings(assign.Spec.ApplyTo),
		path:     path,
	}, nil
}

func applyToToBindings(applyTos []mutationsv1alpha1.ApplyTo) []SchemaBinding {
	res := []SchemaBinding{}
	for _, applyTo := range applyTos {
		binding := SchemaBinding{
			Groups:   make([]string, len(applyTo.Groups)),
			Kinds:    make([]string, len(applyTo.Kinds)),
			Versions: make([]string, len(applyTo.Versions)),
		}
		for i, g := range applyTo.Groups {
			binding.Groups[i] = g
		}
		for i, k := range applyTo.Kinds {
			binding.Kinds[i] = k
		}
		for i, v := range applyTo.Versions {
			binding.Versions[i] = v
		}
		res = append(res, binding)
	}
	return res
}

// IsValidAssign returns an error if the given assign object is not
// semantically valid
func IsValidAssign(assign *mutationsv1alpha1.Assign) error {
	path, err := parser.Parse(assign.Spec.Location)
	if err != nil {
		return errors.Wrap(err, "invalid location format")
	}

	if hasMetadataRoot(path) {
		return errors.New("assign can't change metadata")
	}

	err = checkKeyNotChanged(path)
	if err != nil {
		return err
	}

	toAssign := make(map[string]interface{})
	err = json.Unmarshal([]byte(assign.Spec.Parameters.Assign.Raw), &toAssign)
	if err != nil {
		return errors.Wrap(err, "invalid format for parameters.assign")
	}

	value, ok := toAssign["value"]
	if !ok {
		return errors.New("spec.parameters.assign must have a value field")
	}

	err = validateObjectAssignedToList(path, value)
	if err != nil {
		return err
	}
	return nil
}

func hasMetadataRoot(path *parser.Path) bool {
	if len(path.Nodes) == 0 {
		return false
	}

	if reflect.DeepEqual(path.Nodes[0], &parser.Object{Reference: "metadata"}) {
		return true
	}
	return false
}

// checkKeyNotChanged does not allow to change the key field of
// a list element. A path like foo[name: bar].name is rejected
func checkKeyNotChanged(p *parser.Path) error {
	if len(p.Nodes) == 0 || p.Nodes == nil {
		return errors.New("empty path")
	}
	if len(p.Nodes) < 2 {
		return nil
	}
	lastNode := p.Nodes[len(p.Nodes)-1]
	secondLastNode := p.Nodes[len(p.Nodes)-2]

	if secondLastNode.Type() != parser.ListNode {
		return nil
	}
	if lastNode.Type() != parser.ObjectNode {
		return errors.New("invalid path format: child of a list can't be a list")
	}
	addedObject, ok := lastNode.(*parser.Object)
	if !ok {
		return errors.New("failed converting an ObjectNodeType to Object")
	}
	listNode, ok := secondLastNode.(*parser.List)
	if !ok {
		return errors.New("failed converting a ListNodeType to List")
	}

	if addedObject.Reference == listNode.KeyField {
		return errors.New("invalid path format: changing the item key is not allowed")
	}
	return nil
}

func validateObjectAssignedToList(p *parser.Path, value interface{}) error {
	if len(p.Nodes) == 0 || p.Nodes == nil {
		return errors.New("empty path")
	}
	if p.Nodes[len(p.Nodes)-1].Type() != parser.ListNode {
		return nil
	}
	listNode, ok := p.Nodes[len(p.Nodes)-1].(*parser.List)
	if !ok {
		return errors.New("failed converting a ListNodeType to List")
	}
	if listNode.Glob {
		return errors.New("can't append to a globbed list")
	}
	if listNode.KeyValue == nil {
		return errors.New("invalid key value for a non globbed object")
	}
	valueMap, ok := value.(map[string]interface{})
	if !ok {
		return errors.New("only full objects can be appended to lists")
	}
	if *listNode.KeyValue != valueMap[listNode.KeyField] {
		return fmt.Errorf("adding object to list with different key %s: list key %s, object key %s", listNode.KeyField, *listNode.KeyValue, valueMap[listNode.KeyField])
	}

	return nil
}
