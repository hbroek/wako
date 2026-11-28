# Text and lists

Wako reads **GitHub Flavored Markdown**: CommonMark plus tables, task lists, strikethrough and bare links. You can write *emphasis*, **strong text**, ~~strikethrough~~ and `inline code`.

## Headings

Use `#` to `######`. This page has several levels, and a long page gets an outline from them.

### A third-level heading

#### And a fourth

## Lists

A bulleted list:

- Apples
- Bananas
  - Cavendish
  - Plantain
- Cherries

A numbered list:

1. Preheat the oven
2. Mix the dry ingredients
3. Add the wet ingredients
   1. Stir gently
   2. Do not overmix
4. Bake for 25 minutes

A list with paragraphs and code:

1. Install it

   ```sh
   ./wakod
   ```

2. Open <http://127.0.0.1:8888/> and sign in.

## Quotes

> A wiki is the simplest online database that could possibly work.
>
> — Ward Cunningham

## Code

```go
package main

import "fmt"

func main() {
	fmt.Println("Hello from a wako page")
}
```

## Rules and breaks

A horizontal rule:

---

Back to the [welcome page](welcome).
