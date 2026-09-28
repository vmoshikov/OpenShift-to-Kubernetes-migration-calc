package main

import (
	"html"
	"html/template"
	"regexp"
	"strconv"
	"strings"
)

// Минимальный рендер Markdown → HTML для ответов LLM в веб-UI (stdlib only).
// Поддержано то, что модели реально выдают: заголовки, **жирный**, *курсив*,
// `код`, блоки ```кода```, маркированные/нумерованные списки с вложенностью,
// таблицы, > цитаты, ---, [ссылки](https://...).
//
// Безопасность: ответ модели — недоверенный ввод (а через llm-paste в него
// может попасть prompt injection из вставленного текста). Поэтому весь текст
// сначала HTML-экранируется, разметка добавляется поверх экранированного,
// а ссылки разрешены только http/https.

var (
	mdHeading    = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	mdListItem   = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+(.*)$`)
	mdHR         = regexp.MustCompile(`^\s{0,3}(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
	mdTableSep   = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
	mdCodeSpan   = regexp.MustCompile("`([^`]+)`")
	mdBold       = regexp.MustCompile(`\*\*(\S(?:.*?\S)?)\*\*|__(\S(?:.*?\S)?)__`)
	mdItalic     = regexp.MustCompile(`(^|[^*\w])\*(\S(?:[^*]*?\S)?)\*([^*\w]|$)`)
	mdItalicU    = regexp.MustCompile(`(^|[^_\w])_(\S(?:[^_]*?\S)?)_([^_\w]|$)`)
	mdLink       = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s)]+)\)`)
	mdCodeHolder = regexp.MustCompile("\x00(\\d+)\x00")
)

// renderMarkdown превращает Markdown в безопасный HTML.
func renderMarkdown(src string) template.HTML {
	r := &mdRenderer{}
	r.render(strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n"))
	return template.HTML(r.out.String())
}

type mdList struct {
	tag    string // ul | ol
	indent int
}

type mdRenderer struct {
	out   strings.Builder
	para  []string
	lists []mdList
}

func (r *mdRenderer) render(lines []string) {
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "```"):
			r.flush()
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			r.out.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")

		case trimmed == "":
			r.flushPara()
			// пустая строка внутри списка не закрывает его, если дальше снова пункт
			if len(r.lists) > 0 && (i+1 >= len(lines) || !mdListItem.MatchString(lines[i+1])) {
				r.closeLists()
			}

		case mdHR.MatchString(line) && !mdListItem.MatchString(line):
			r.flush()
			r.out.WriteString("<hr>\n")

		case mdHeading.MatchString(trimmed):
			r.flush()
			m := mdHeading.FindStringSubmatch(trimmed)
			// # → h3: заголовки ответа не должны перекрикивать заголовки страницы
			lvl := len(m[1]) + 2
			if lvl > 6 {
				lvl = 6
			}
			tag := "h" + string(rune('0'+lvl))
			r.out.WriteString("<" + tag + ">" + mdInline(m[2]) + "</" + tag + ">\n")

		case strings.HasPrefix(trimmed, "|") && i+1 < len(lines) && mdTableSep.MatchString(lines[i+1]):
			r.flush()
			i = r.table(lines, i)

		case strings.HasPrefix(trimmed, ">"):
			r.flush()
			var quote []string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">"); i++ {
				quote = append(quote, strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">"), " "))
			}
			i--
			inner := &mdRenderer{}
			inner.render(quote)
			r.out.WriteString("<blockquote>" + inner.out.String() + "</blockquote>\n")

		case mdListItem.MatchString(line):
			r.flushPara()
			m := mdListItem.FindStringSubmatch(line)
			r.listItem(len(strings.ReplaceAll(m[1], "\t", "    ")), m[2], m[3])

		default:
			if len(r.lists) > 0 && (line[0] == ' ' || line[0] == '\t') {
				// продолжение пункта списка на следующей строке
				r.out.WriteString(" " + mdInline(trimmed))
				continue
			}
			r.closeLists()
			r.para = append(r.para, trimmed)
		}
	}
	r.flush()
}

func (r *mdRenderer) listItem(indent int, marker, text string) {
	tag := "ul"
	if marker[0] >= '0' && marker[0] <= '9' {
		tag = "ol"
	}
	// закрыть более глубокие уровни
	for len(r.lists) > 0 && r.lists[len(r.lists)-1].indent > indent {
		r.closeTop()
	}
	top := len(r.lists) - 1
	switch {
	case top >= 0 && r.lists[top].indent == indent && r.lists[top].tag == tag:
		r.out.WriteString("</li>\n")
	case top >= 0 && r.lists[top].indent == indent:
		// тот же уровень, другой тип списка
		r.closeTop()
		r.openList(tag, indent)
	default:
		// первый уровень или вложенный внутрь текущего <li>
		r.openList(tag, indent)
	}
	r.out.WriteString("<li>" + mdInline(text))
}

func (r *mdRenderer) openList(tag string, indent int) {
	r.out.WriteString("<" + tag + ">\n")
	r.lists = append(r.lists, mdList{tag: tag, indent: indent})
}

func (r *mdRenderer) closeTop() {
	l := r.lists[len(r.lists)-1]
	r.out.WriteString("</li>\n</" + l.tag + ">\n")
	r.lists = r.lists[:len(r.lists)-1]
}

func (r *mdRenderer) closeLists() {
	for len(r.lists) > 0 {
		r.closeTop()
	}
}

func (r *mdRenderer) flushPara() {
	if len(r.para) == 0 {
		return
	}
	r.out.WriteString("<p>" + mdInline(strings.Join(r.para, " ")) + "</p>\n")
	r.para = nil
}

func (r *mdRenderer) flush() {
	r.flushPara()
	r.closeLists()
}

// table рендерит таблицу, начиная со строки заголовка i; возвращает индекс
// последней строки таблицы.
func (r *mdRenderer) table(lines []string, i int) int {
	r.out.WriteString("<table>\n<tr>")
	for _, c := range mdCells(lines[i]) {
		r.out.WriteString("<th>" + mdInline(c) + "</th>")
	}
	r.out.WriteString("</tr>\n")
	i += 2 // заголовок + разделитель
	for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
		r.out.WriteString("<tr>")
		for _, c := range mdCells(lines[i]) {
			r.out.WriteString("<td>" + mdInline(c) + "</td>")
		}
		r.out.WriteString("</tr>\n")
	}
	r.out.WriteString("</table>\n")
	return i - 1
}

func mdCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|")
	cells := strings.Split(line, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// mdInline — строчная разметка поверх экранированного текста. Содержимое
// `кода` прячется в плейсхолдеры, чтобы * и _ внутри него не трогать.
func mdInline(s string) string {
	s = html.EscapeString(s)
	var codes []string
	s = mdCodeSpan.ReplaceAllStringFunc(s, func(m string) string {
		codes = append(codes, "<code>"+m[1:len(m)-1]+"</code>")
		return "\x00" + strconv.Itoa(len(codes)-1) + "\x00"
	})
	s = mdLink.ReplaceAllString(s, `<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>`)
	s = mdBold.ReplaceAllString(s, "<strong>$1$2</strong>")
	// Повтор до неподвижной точки: соседние *a* *b* делят разделитель, и за
	// один проход ReplaceAll второй не найдёт.
	for _, re := range []*regexp.Regexp{mdItalic, mdItalicU} {
		for prev := ""; prev != s; {
			prev = s
			s = re.ReplaceAllString(s, "$1<em>$2</em>$3")
		}
	}
	return mdCodeHolder.ReplaceAllStringFunc(s, func(m string) string {
		idx, err := strconv.Atoi(m[1 : len(m)-1])
		if err == nil && idx < len(codes) {
			return codes[idx]
		}
		return m
	})
}
