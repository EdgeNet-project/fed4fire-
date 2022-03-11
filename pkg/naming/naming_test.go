package naming

import (
	"testing"

	"github.com/EdgeNet-project/fed4fire/pkg/identifiers"
	"k8s.io/apimachinery/pkg/util/validation"
)

var testSliceIdentifier = identifiers.MustParse("urn:publicid:IDN+example.org+slice+test")

func TestSliceHash(t *testing.T) {
	h := SliceHash(testSliceIdentifier.URN())
	errs := validation.IsValidLabelValue(h)
	if len(errs) > 0 {
		t.Errorf("%s", errs)
	}
}

func TestSliverName(t *testing.T) {
	h := SliverName(testSliceIdentifier.URN(), "Client$Id&*")
	errs := validation.IsValidLabelValue(h)
	if len(errs) > 0 {
		t.Errorf("%s", errs)
	}
}
