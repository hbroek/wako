---
title: Front matter and macros
author: Alex Chen
team: Platform
tags: [demo, macros, docs]
reviewed: 2026-11-01
---

# {{page.title}}

The block of `key: value` lines at the top of this page is front matter. It is never shown, it sets the page title, and every key can be used in the text.

## The built-in macros

| Macro | Value on this page |
| --- | --- |
| `{{page.title}}` | {{page.title}} |
| `{{page.path}}` | {{page.path}} |
| `{{page.slug}}` | {{page.slug}} |
| `{{page.space}}` | {{page.space}} |

## Your own keys

Any key in the front matter works as `page.KEY`:

- Written by **{{page.author}}** for the **{{page.team}}** team
- Tags: {{page.tags}}
- Last reviewed on {{page.reviewed}}

A name that does not exist stays as written: {{page.nothing}}.

To show a macro without filling it in, put a backslash in front of it: \{{page.title}}. Macros inside code are never filled in either:

```text
{{page.title}}
```

Values are always plain text, so a value like `**bold** [x](http://evil)` can never turn into markup.

Back to the [welcome page](welcome).
