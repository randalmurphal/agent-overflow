package userquestion

import "testing"

func TestStructuredQuestionsRequireValidAsyncMetadata(t *testing.T) {
	for _, raw := range []string{
		`{"delivery":"async","questions":[]}`,
		`{"questions":[{"title":"Which?"}]}`,
		`{"delivery":"async","questions":[{"title":" "}]}`,
		`{"delivery":"async","questions":[{"title":"Which?","options":[]}]}`,
		`{"delivery":"async","questions":[{"title":"Which?","options":[" "]}]}`,
	} {
		if _, found, err := Decode([]byte(raw)); !found || err == nil {
			t.Fatalf("accepted invalid questions: %s", raw)
		}
	}
	for _, raw := range []string{`{}`, `{"delivery":"async"}`, `{"delivery":"async","questions":null}`} {
		if _, found, err := Decode([]byte(raw)); found || err != nil {
			t.Fatalf("inferred questions from prose: %s", raw)
		}
	}
	meta, found, err := Decode([]byte(`{"delivery":"async","questions":[{"title":"Which?","options":["A","B"]},{"title":"Anything else?"}]}`))
	if err != nil || !found || len(meta.Questions) != 2 || meta.Questions[1].Options != nil {
		t.Fatalf("valid questions lost: %+v %v", meta, err)
	}
}
