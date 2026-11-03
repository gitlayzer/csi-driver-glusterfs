package gluster

import "testing"

func TestShellQuote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "empty argument list",
			args: nil,
			want: "",
		},
		{
			name: "single argument without special characters is still quoted",
			args: []string{"gluster"},
			want: "'gluster'",
		},
		{
			name: "several arguments are separated by one space",
			args: []string{"gluster", "--mode=script", "--xml"},
			want: "'gluster' '--mode=script' '--xml'",
		},
		{
			name: "empty string becomes an empty quoted word",
			args: []string{""},
			want: "''",
		},
		{
			name: "empty string between two arguments",
			args: []string{"a", "", "b"},
			want: "'a' '' 'b'",
		},
		{
			name: "single quote inside a word",
			args: []string{"it's"},
			want: `'it'\''s'`,
		},
		{
			name: "only single quotes",
			args: []string{"'''"},
			want: `''\'''\'''\'''`,
		},
		{
			name: "several arguments with quotes",
			args: []string{"a'b", "c''d"},
			want: `'a'\''b' 'c'\'''\''d'`,
		},
		{
			name: "space is inside the quotes",
			args: []string{"a b", " c ", "\t"},
			want: "'a b' ' c ' '\t'",
		},
		{
			name: "shell metacharacters are not special inside single quotes",
			args: []string{"$(id)", "`id`", "; id", "a|b", "a&b", "a>b", "*", "~", `\`, "!", "$HOME", "${IFS}"},
			want: "'$(id)' '`id`' '; id' 'a|b' 'a&b' 'a>b' '*' '~' '\\' '!' '$HOME' '${IFS}'",
		},
		{
			name: "non ASCII argument",
			args: []string{"卷-1"},
			want: "'卷-1'",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shellQuote(tc.args); got != tc.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
