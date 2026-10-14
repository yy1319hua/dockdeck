// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.3：diff.Unified 的契约测试——输出需与 `diff -u` 的 hunk 头语义一致。

package diff

import "testing"

func TestUnified(t *testing.T) {
	cases := []struct {
		name    string
		oldText string
		newText string
		want    string
	}{
		{
			name:    "identical",
			oldText: "a\nb\nc\n",
			newText: "a\nb\nc\n",
			want:    "",
		},
		{
			name:    "trailing-newline-insensitive",
			oldText: "a\nb\n",
			newText: "a\nb",
			want:    "",
		},
		{
			name:    "pure-addition-at-top",
			oldText: "",
			newText: "X\nY\n",
			want:    "@@ -0,0 +1,2 @@\n+ X\n+ Y\n",
		},
		{
			name:    "middle-insertion",
			oldText: "a\nb\nc\n",
			newText: "a\nX\nb\nc\n",
			want:    "@@ -1,3 +1,4 @@\n a\n+ X\n b\n c\n",
		},
		{
			name:    "middle-deletion",
			oldText: "a\nb\nc\n",
			newText: "a\nc\n",
			want:    "@@ -1,3 +1,2 @@\n a\n- b\n c\n",
		},
		{
			name:    "modification",
			oldText: "a\nb\nc\n",
			newText: "a\nB\nc\n",
			want:    "@@ -1,3 +1,3 @@\n a\n- b\n+ B\n c\n",
		},
		{
			name:    "multi-block-merge-when-close",
			oldText: "a\nb\nc\nd\ne\nf\n",
			newText: "a\nX\nc\nd\nY\nf\n",
			want:    "@@ -1,6 +1,6 @@\n a\n- b\n+ X\n c\n d\n- e\n+ Y\n f\n",
		},
		{
			name:    "multi-block-separate-when-far",
			oldText: "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\n",
			newText: "a\nX\nc\nd\ne\nf\ng\nh\ni\nY\nk\n",
			want:    "@@ -1,5 +1,5 @@\n a\n- b\n+ X\n c\n d\n e\n@@ -7,5 +7,5 @@\n g\n h\n i\n- j\n+ Y\n k\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Unified(c.oldText, c.newText)
			if got != c.want {
				t.Errorf("Unified 不符:\n got=%q\nwant=%q", got, c.want)
			}
		})
	}
}
