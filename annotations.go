package main

// Graphic annotations — the arrows, lines, outlines, measurements and text
// boxes a reviewer draws over an image. DICOM does not store them in the image:
// they are vectors in a presentation state (modality PR, group 0070), a
// separate object that points at the images by UID and tells a viewer what to
// draw over them. Burned-in text and overlay planes are other channels, reached
// by pixel masking and Remove overlay planes.
//
// A profile's `annotations` setting (dicomqr-only, profile-wide) decides what
// an export keeps of them:
//
//   - keep (the default, stored empty): nothing is done here, and the profile's
//     tag rules apply as written.
//   - notext: the shapes stay and every piece of text goes — the text boxes
//     (Text Object Sequence, with the Unformatted Text Value each holds), layer
//     descriptions and group descriptions. A shape is coordinates and carries
//     no identity; text is whatever the reviewer typed. The cost is that a
//     measurement's number goes with its label, though it can be recomputed
//     from the shape's coordinates. Tick labels on a ruler or axis graphic stay,
//     as part of the shape.
//   - remove: every annotation goes, shapes and text alike.
//
// Neither mode touches what a presentation state says about display — window
// and level, zoom, rotation, shutters — or its identification (Content Label,
// creator, creation date), which are the tag rules' business.
//
// Removing text leaves the object valid where the standard requires a value
// (PS3.3 C.10.5, C.10.11): an annotation item left with nothing to draw is
// dropped rather than kept empty, and the Graphic Annotation Sequence goes when
// no item remains; a Graphic Group Label, which is required, is replaced with a
// neutral "GROUP n" rather than removed.

import (
	"fmt"
	"slices"
	"strings"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// The stored tokens. Keep is the empty value, so a profile written before the
// setting existed — or by dicomtool, which has no such field — means what it
// always meant.
const (
	annotationsKeep   = ""
	annotationsNoText = "notext"
	annotationsRemove = "remove"
)

// annotationMode normalises a stored token. "keep" is accepted as a spelling of
// the empty default. ok is false for anything else, which only a hand-edited
// profiles.json can hold; compileModifyParams refuses it rather than guess.
func annotationMode(token string) (mode string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "", "keep":
		return annotationsKeep, true
	case annotationsNoText:
		return annotationsNoText, true
	case annotationsRemove:
		return annotationsRemove, true
	}
	return "", false
}

// Labels for the choice, shared by the profile editor and the Modification
// dialog so the two Options blocks read the same.
const (
	annotationsRowLabel    = "Graphic annotations"
	annotationsLabelKeep   = "Keep"
	annotationsLabelNoText = "Keep shapes, remove text"
	annotationsLabelRemove = "Remove"
)

// annotationsLabels is the option list, in the order the selects present it.
var annotationsLabels = []string{annotationsLabelKeep, annotationsLabelNoText, annotationsLabelRemove}

// annotationsLabelFor maps a stored token to its label. An unrecognised token
// shows as Remove, the strictest choice, so saving the profile from the editor
// can only tighten what its author wrote, never loosen it; until then
// compileModifyParams refuses to run it.
func annotationsLabelFor(token string) string {
	mode, ok := annotationMode(token)
	switch {
	case !ok, mode == annotationsRemove:
		return annotationsLabelRemove
	case mode == annotationsNoText:
		return annotationsLabelNoText
	}
	return annotationsLabelKeep
}

// annotationsFromLabel is the inverse, mapping a selected label to the token
// stored in the profile.
func annotationsFromLabel(label string) string {
	switch label {
	case annotationsLabelNoText:
		return annotationsNoText
	case annotationsLabelRemove:
		return annotationsRemove
	}
	return annotationsKeep
}

// annotationTextTags are what notext removes, at any depth: the text boxes
// (each carrying its text, anchor and leader line), the text itself wherever it
// sits, and the free-text descriptions of layers and groups.
var annotationTextTags = map[tag.Tag]bool{
	tag.TextObjectSequence:      true,
	tag.UnformattedTextValue:    true,
	tag.GraphicLayerDescription: true,
	tag.GraphicGroupDescription: true,
}

// annotationRemoveTags are what remove removes, at any depth: the text above
// plus every shape — the annotation sequence that holds them, graphic and
// compound-graphic objects found anywhere else, and the groups binding them.
// The Graphic Layer Sequence stays: overlay planes can be assigned to its
// layers, and a layer list says nothing about anyone.
var annotationRemoveTags = func() map[tag.Tag]bool {
	m := map[tag.Tag]bool{
		tag.GraphicAnnotationSequence: true,
		tag.GraphicObjectSequence:     true,
		tag.CompoundGraphicSequence:   true,
		tag.GraphicGroupSequence:      true,
		tag.GraphicGroupLabel:         true,
	}
	for t := range annotationTextTags {
		m[t] = true
	}
	return m
}()

// applyAnnotationMode applies a compiled annotation mode to elements, at every
// nesting depth, returning the elements to keep. Keep returns them untouched.
func applyAnnotationMode(elements []*sdicom.Element, mode string) []*sdicom.Element {
	switch mode {
	case annotationsNoText:
		return pruneAnnotations(elements, annotationTextTags, true)
	case annotationsRemove:
		return pruneAnnotations(elements, annotationRemoveTags, false)
	}
	return elements
}

// pruneAnnotations drops every element whose tag is in drop and recurses into
// the sequences it keeps, in the in-place style of pruneElements. With
// removingText it also repairs what removing text would leave invalid: an
// annotation item with nothing left to draw is dropped, the annotation
// sequence goes with its last item, and each group label is neutralised.
func pruneAnnotations(elements []*sdicom.Element, drop map[tag.Tag]bool, removingText bool) []*sdicom.Element {
	out := elements[:0]
	for _, elem := range elements {
		if elem == nil {
			continue
		}
		if drop[elem.Tag] {
			continue
		}
		if elem.Value != nil && elem.Value.ValueType() == sdicom.Sequences {
			if items, ok := elem.Value.GetValue().([]*sdicom.SequenceItemValue); ok {
				rebuilt := make([][]*sdicom.Element, 0, len(items))
				for _, item := range items {
					var sub []*sdicom.Element
					if item != nil {
						sub, _ = item.GetValue().([]*sdicom.Element)
					}
					sub = pruneAnnotations(sub, drop, removingText)
					if removingText && elem.Tag == tag.GraphicAnnotationSequence &&
						!hasSequenceItems(sub, tag.GraphicObjectSequence) &&
						!hasSequenceItems(sub, tag.CompoundGraphicSequence) {
						continue // a text-only annotation: nothing left to draw
					}
					if removingText && elem.Tag == tag.GraphicGroupSequence {
						neutraliseGroupLabel(sub, len(rebuilt)+1)
					}
					rebuilt = append(rebuilt, sub)
				}
				if removingText && elem.Tag == tag.GraphicAnnotationSequence && len(rebuilt) == 0 {
					continue // the module is optional; an empty sequence is not
				}
				if v, err := sdicom.NewValue(rebuilt); err == nil {
					elem.Value = v
				}
			}
		}
		out = append(out, elem)
	}
	return out
}

// hasSequenceItems reports whether elements hold a sequence t with at least
// one item.
func hasSequenceItems(elements []*sdicom.Element, t tag.Tag) bool {
	for _, e := range elements {
		if e == nil || e.Tag != t || e.Value == nil || e.Value.ValueType() != sdicom.Sequences {
			continue
		}
		if items, ok := e.Value.GetValue().([]*sdicom.SequenceItemValue); ok && len(items) > 0 {
			return true
		}
	}
	return false
}

// neutraliseGroupLabel replaces a Graphic Group Sequence item's label — a name
// a reviewer may have typed, but a required one — with "GROUP <id>", using the
// item's own Graphic Group ID where it has one and its position otherwise. The
// element is rewritten in place, keeping its tag and position.
func neutraliseGroupLabel(item []*sdicom.Element, ordinal int) {
	n := ordinal
	if i := slices.IndexFunc(item, func(e *sdicom.Element) bool { return e != nil && e.Tag == tag.GraphicGroupID }); i >= 0 {
		if ids, ok := item[i].Value.GetValue().([]int); ok && len(ids) > 0 {
			n = ids[0]
		}
	}
	for _, e := range item {
		if e == nil || e.Tag != tag.GraphicGroupLabel {
			continue
		}
		if repl, err := sdicom.NewElement(tag.GraphicGroupLabel, []string{fmt.Sprintf("GROUP %d", n)}); err == nil {
			e.Value = repl.Value
		}
	}
}

// annotationShapeTags are the tags whose removal takes an annotation's shapes
// with it: the sequences holding them, their geometry, and the layers every
// annotation is drawn on. A profile that keeps shapes but lists one of these in
// Remove tags keeps nothing.
var annotationShapeTags = map[tag.Tag]bool{
	tag.GraphicAnnotationSequence: true,
	tag.GraphicLayer:              true,
	tag.GraphicAnnotationUnits:    true,
	tag.GraphicObjectSequence:     true,
	tag.GraphicDimensions:         true,
	tag.NumberOfGraphicPoints:     true,
	tag.GraphicData:               true,
	tag.GraphicType:               true,
	tag.GraphicLayerSequence:      true,
	tag.CompoundGraphicSequence:   true,
}

// annotationTextBoxTags are the text boxes and everything that places them. A
// profile that keeps annotations whole but lists one of these in Remove tags
// loses its text, or leaves the box unplaceable.
var annotationTextBoxTags = map[tag.Tag]bool{
	tag.TextObjectSequence:                     true,
	tag.UnformattedTextValue:                   true,
	tag.BoundingBoxAnnotationUnits:             true,
	tag.AnchorPointAnnotationUnits:             true,
	tag.BoundingBoxTopLeftHandCorner:           true,
	tag.BoundingBoxBottomRightHandCorner:       true,
	tag.BoundingBoxTextHorizontalJustification: true,
	tag.AnchorPoint:                            true,
	tag.AnchorPointVisibility:                  true,
}

// modProfileAnnotationAdvisory returns the note for a profile whose other
// settings undo its annotations choice, or "" when there is nothing to say.
// Two things can: Remove tags naming a tag the choice keeps — the tag rules run
// regardless, and a profile written before this setting existed removed
// annotations that way — and Ignore modalities skipping PR, the modality
// presentation states carry, so none is exported at all. Remove has nothing to
// be undone. Like the birth-date advisory, it reads the profile-level lists
// only: an override's removals reach one modality.
func modProfileAnnotationAdvisory(p ModProfile) string {
	mode, ok := annotationMode(p.Annotations)
	if !ok || mode == annotationsRemove {
		return ""
	}
	var hits []string
	for _, r := range p.Removes {
		t, err := parseTagString(strings.TrimSpace(r))
		if err != nil {
			continue
		}
		if annotationShapeTags[t] || (mode == annotationsKeep && annotationTextBoxTags[t]) {
			hits = append(hits, formatTagRef(t))
		}
	}
	prSkipped := slices.ContainsFunc(p.IgnoreModalities, func(m string) bool {
		return strings.EqualFold(strings.TrimSpace(m), "PR")
	})

	var parts []string
	if len(hits) > 0 {
		kept := "the annotations"
		if mode == annotationsNoText {
			kept = "the shapes"
		}
		parts = append(parts, fmt.Sprintf("Remove tags lists %s, which take %s out of every file whatever "+
			"Graphic annotations says. Take %s out of Remove tags for this setting to apply.",
			joinTagRefs(hits), kept, itThem(len(hits))))
	}
	if prSkipped {
		parts = append(parts, "Ignore modalities skips PR, and presentation states — where graphic "+
			"annotations are stored — have that modality, so none is exported at all.")
	}
	return strings.Join(parts, " ")
}

// joinTagRefs lists tag references as "A", "A and B" or "A, B and C".
func joinTagRefs(refs []string) string {
	switch len(refs) {
	case 0:
		return ""
	case 1:
		return refs[0]
	}
	return strings.Join(refs[:len(refs)-1], ", ") + " and " + refs[len(refs)-1]
}
