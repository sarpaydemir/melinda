package plane

import "testing"

func TestCommentMarkupBecomesText(t *testing.T) {
	for _, c := range []struct{ markup, want string }{
		{"<p></p>\n\n\n\n<p>a \t\nb</p><BR >c<br>",
			"a\nb\n\nc"},
		{"<blockquote>q</BLOCKQUOTE>after</Div>x</oL>y</UL>",
			"q\nafter\nx\ny"},
		{"< br / >a<br\x1c>b<br\u00a0/>c<br\u200b>d",
			"a\nb\ncd"},
		{"< / p >a</ p\u0085>b</pre>c",
			"a\nbc"},
		{"<li>a</li><l\u0130>b</l\u0130><l\u0131>c</LI><li-x>d<li\u00e9>e<li\u0301>f<li_x>g<list>h<li",
			"* a\n* b\n* c\n* de* fgh<li"},
		{"</d\u0130v>a</bloc\u212aquote>b",
			"a\nb"},
		{"x &#9; &#x41;&#65 &#0; &#1; &#128; &#x81; &#55296; &#1114112; &#xFFFE; &#11; &#127; &#13;y",
			"x \t AA \ufffd  \u20ac \u0081 \ufffd \ufffd    \ry"},
		{"&#9&#x9z &#2z &notit; &amp &ampx &nGt; &aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa;",
			"z z \u00acit; & &x \u226b\u20d2 &aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa;"},
		{"a <b>x</b> c > d < e",
			"a x c > d < e"},
		{"  \n line \t \n\n\n\n end \u3000",
			"line\n\n end"},
		{"\u00a0\n\x1c",
			""},
		{"unclosed <p",
			"unclosed <p"},
	} {
		if got := commentText(c.markup); got != c.want {
			t.Errorf("commentText(%q) = %q, want %q", c.markup, got, c.want)
		}
	}
}
