// Flat config (ESLint 9). This project shipped a `lint` script for a linter
// that was never installed and had no config anywhere, so `npm run lint` exited
// 127 while the code was written as if it ran — NotesView.vue still carries an
// `eslint-disable-next-line vue/no-v-html`. This file makes the gate real.
//
// Scope note: this is deliberately NOT `recommendedTypeChecked`. That preset
// turns on the whole no-unsafe-* family, which on a codebase this size buries
// the findings that matter under thousands of `any`-flavoured warnings. The
// point here is the promise/await bug class the type checker cannot see, so we
// take the plain recommended presets and add the type-aware promise rules
// explicitly. Widening later is one line.
import js from '@eslint/js'
import tseslint from 'typescript-eslint'
import pluginVue from 'eslint-plugin-vue'
import globals from 'globals'

export default tseslint.config(
  {
    ignores: [
      'dist/**',
      'build/**',
      'node_modules/**',
      'coverage/**',
      // Generated from the OpenAPI spec by `npm run openapi:types`; linting it
      // reports on the generator, not on anything anyone can fix here.
      'src/api/openapi-types.ts',
    ],
  },

  js.configs.recommended,
  ...tseslint.configs.recommended,
  ...pluginVue.configs['flat/recommended'],

  {
    files: ['**/*.{ts,vue}'],
    languageOptions: {
      parserOptions: {
        // eslint-plugin-vue puts vue-eslint-parser in front; this is the parser
        // it hands the <script> block to.
        parser: tseslint.parser,
        // projectService resolves each file to the right tsconfig on its own,
        // which is what makes the type-aware rules below possible.
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
        extraFileExtensions: ['.vue'],
      },
      globals: { ...globals.browser },
    },
    rules: {
      // The reason this gate exists. vue-tsc cannot see either of these: an
      // un-awaited promise that rejects is an unhandled rejection, and in an
      // async DOM handler that means the action silently does nothing (exactly
      // the FileTabBar drop bug). Note this codebase has a deliberate
      // fire-and-forget pattern via safeCall(), so expect real hits here —
      // marking those sites with `void` is the intended resolution, not
      // switching the rule off.
      '@typescript-eslint/no-floating-promises': 'error',
      // An async function passed where a void-returning callback is expected:
      // the caller cannot await it, so its rejection escapes.
      '@typescript-eslint/no-misused-promises': 'error',

      // Unused code, kept as warnings so it never blocks the error gate.
      // `_`-prefixed names are the escape hatch for intentionally-unused args.
      '@typescript-eslint/no-unused-vars': ['warn', {
        argsIgnorePattern: '^_',
        varsIgnorePattern: '^_',
        caughtErrorsIgnorePattern: '^_',
      }],
      'vue/no-unused-vars': 'warn',
    },
  },

  {
    // TypeScript already resolves identifiers, and it understands the DOM and
    // Vue macros that no-undef does not. Leaving it on produces false positives
    // on things like `defineProps`.
    files: ['**/*.{ts,vue}'],
    rules: { 'no-undef': 'off' },
  },

  {
    // eslint-plugin-vue's "strongly-recommended" tier is mostly a formatter
    // wearing a linter's clothes: attribute-per-line, indentation, where line
    // breaks go inside a tag. On this codebase those seven rules alone produced
    // 4448 of 4568 warnings — a 27:1 noise ratio that buries the 152 real
    // promise bugs the gate exists to find, and nobody reads a report they have
    // to scroll past 4000 whitespace complaints to use. Whitespace belongs to a
    // formatter (Prettier), not to the gate. Everything semantic in that tier
    // stays on.
    files: ['**/*.vue'],
    rules: {
      'vue/max-attributes-per-line': 'off',
      'vue/html-indent': 'off',
      'vue/singleline-html-element-content-newline': 'off',
      'vue/multiline-html-element-content-newline': 'off',
      'vue/html-self-closing': 'off',
      'vue/first-attribute-linebreak': 'off',
      'vue/html-closing-bracket-newline': 'off',
    },
  },

  {
    // Root-level build config is outside tsconfig's `include`, so the type-aware
    // project service cannot resolve it and every rule there dies with a parse
    // error. It needs no type information, so lint it without the project.
    files: ['*.config.{js,ts}', 'vite.config.ts'],
    languageOptions: {
      parserOptions: { projectService: false, project: false },
      globals: { ...globals.node },
    },
    rules: {
      '@typescript-eslint/no-floating-promises': 'off',
      '@typescript-eslint/no-misused-promises': 'off',
    },
  },

  {
    // Test files and build scripts run under Node, not the browser.
    files: ['**/*.spec.ts', '**/*.test.ts', 'scripts/**/*.ts', '*.config.{js,ts}'],
    languageOptions: { globals: { ...globals.node } },
  },
)
