package main

import (
	"strings"
	"testing"
)

func TestRenderMarkdown(t *testing.T) {
	tests := []struct {
		name, in string
		want     []string // подстроки, которые должны быть
		notWant  []string // подстроки, которых быть не должно
	}{
		{"XSS экранируется", `<script>alert(1)</script> **жирный <img src=x onerror=alert(1)>**`,
			[]string{"&lt;script&gt;", "<strong>жирный &lt;img"}, []string{"<script>", "<img"}},
		{"javascript: в ссылке не становится ссылкой", `[клик](javascript:alert(1)) и [док](https://k8s.io/docs?a=1&b=2)`,
			[]string{`<a href="https://k8s.io/docs?a=1&amp;b=2"`}, []string{`href="javascript`}},
		{"заголовки ниже заголовков страницы", "# Итог\n## Риски\n### Детали",
			[]string{"<h3>Итог</h3>", "<h4>Риски</h4>", "<h5>Детали</h5>"}, nil},
		{"жирный, курсив, код", "**Риск:** *мягко* и _тоже_, `limits.cpu` и `a*b*c`, snake_case_name",
			[]string{"<strong>Риск:</strong>", "<em>мягко</em>", "<em>тоже</em>", "<code>limits.cpu</code>", "<code>a*b*c</code>", "snake_case_name"}, nil},
		{"соседние курсивы", "*a* *b*", []string{"<em>a</em> <em>b</em>"}, nil},
		{"больше 10 фрагментов кода", strings.Repeat("`x` ", 12), []string{strings.Repeat("<code>x</code> ", 11) + "<code>x</code>"}, []string{"\x00"}},
		{"нумерованный список с вложенным", "1. Первый\n   - деталь а\n   - деталь б\n2. Второй\n\nАбзац",
			[]string{"<ol>\n<li>Первый<ul>\n<li>деталь а</li>\n<li>деталь б</li>\n</ul>\n</li>\n<li>Второй</li>\n</ol>", "<p>Абзац</p>"}, nil},
		{"список с пустыми строками между пунктами", "- a\n\n- b", []string{"<ul>\n<li>a</li>\n<li>b</li>\n</ul>"}, nil},
		{"продолжение пункта", "- первая строка\n  вторая строка", []string{"<li>первая строка вторая строка</li>"}, nil},
		{"таблица", "| Контур | Нод |\n|---|:---:|\n| ПРОМ | **3** |",
			[]string{"<th>Контур</th><th>Нод</th>", "<td>ПРОМ</td><td><strong>3</strong></td>"}, nil},
		{"блок кода без разметки внутри", "```yaml\nlimits:\n  cpu: **2**\n<b>\n```",
			[]string{"<pre><code>limits:\n  cpu: **2**\n&lt;b&gt;</code></pre>"}, []string{"<strong>"}},
		{"цитата и разделитель", "> **Важно:** без метрик\n\n---", []string{"<blockquote><p><strong>Важно:</strong> без метрик</p>\n</blockquote>", "<hr>"}, nil},
		{"абзацы склеиваются", "строка один\nстрока два\n\nновый", []string{"<p>строка один строка два</p>", "<p>новый</p>"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(renderMarkdown(tt.in))
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("нет %q в:\n%s", w, got)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(got, w) {
					t.Errorf("есть лишнее %q в:\n%s", w, got)
				}
			}
		})
	}
}
