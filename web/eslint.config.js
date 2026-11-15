import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import jsxA11y from 'eslint-plugin-jsx-a11y'
import tseslint from 'typescript-eslint'
import { defineConfig, globalIgnores } from 'eslint/config'

export default defineConfig([
  globalIgnores(['dist']),
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
    ],
    plugins: { 'jsx-a11y': jsxA11y },
    // The shadcn wrappers are the elements, as far as these rules are
    // concerned. Without this the plugin sees `<Input>` and `<Button>` as
    // unknown components and checks nothing, which is most of `web/src`.
    settings: {
      'jsx-a11y': {
        components: {
          Input: 'input',
          Textarea: 'textarea',
          Label: 'label',
          Button: 'button',
          Checkbox: 'input',
          Switch: 'input',
        },
      },
    },
    // The accessibility floor, enforced rather than remembered.
    //
    // Not the plugin's whole recommended set: most of it is about elements this
    // codebase does not hand-write, and a rule that fires on nothing is noise
    // in a config somebody has to read. These are the ones a review of the
    // whole of `web/src` found broken — an unlabelled input, an icon button
    // with no name, a row expander that every row named identically, an
    // interactive `<div>` with no keyboard route to it. Each of them is a
    // control that works for some people and does not exist for others.
    rules: {
      'jsx-a11y/label-has-associated-control': ['error', { assert: 'either' }],
      // Buttons and links only. A form control's label is the sibling `<Label
      // htmlFor>` beside it, which is the *other* rule's business — checking
      // for it here would flag every correctly-labelled field in the product.
      // What this one is for is the icon-only button: a `⋯` with no accessible
      // name is a control that does not exist to a screen reader, and the
      // review found thirteen of them.
      'jsx-a11y/control-has-associated-label': [
        'error',
        {
          ignoreElements: ['input', 'textarea', 'select', 'audio', 'video', 'img', 'area'],
          depth: 4,
        },
      ],
      'jsx-a11y/click-events-have-key-events': 'error',
      'jsx-a11y/no-static-element-interactions': 'error',
    },
    languageOptions: {
      ecmaVersion: 2020,
      globals: globals.browser,
    },
  },
  {
    // The shadcn primitives are the parts a caller assembles, so they cannot
    // carry their own labels: `Label` renders the `<label>` the *page* points
    // at a control, `Dialog`'s close and `Pagination`'s arrows are named by
    // their `sr-only` children, and `InputGroup`'s addon forwards a click to
    // the input it sits beside. Each of those is correct and none of them can
    // be expressed to these rules — which is why the exception is scoped to
    // the directory the primitives live in rather than sprinkled inline.
    files: ['src/components/ui/**/*.tsx'],
    rules: {
      'jsx-a11y/label-has-associated-control': 'off',
      'jsx-a11y/control-has-associated-label': 'off',
      'jsx-a11y/click-events-have-key-events': 'off',
    },
  },
])
