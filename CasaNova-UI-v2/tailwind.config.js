/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{vue,js,ts,jsx,tsx}'],
  darkMode: 'class',
  theme: {
    extend: {
      // Alias the Tailwind `blue` ramp to the runtime `--cn-accent`
      // variables written by the theme store. Every component that
      // uses Tailwind classes like `bg-blue-500`, `text-blue-500`,
      // `border-blue-500`, `ring-blue-500` now picks up the user's
      // accent colour automatically without any component-level
      // refactor. The 500 stop is the primary accent; the hover stop
      // is slightly darker; the others keep sensible fallbacks for
      // the rare utility that references them.
      //
      // Using the `<alpha-value>` placeholder so Tailwind's slash-
      // alpha syntax (`bg-blue-500/10`, `text-blue-500/80`) keeps
      // working against the CSS-variable-backed color.
      colors: {
        blue: {
          50: 'rgb(239 246 255 / <alpha-value>)',
          100: 'rgb(219 234 254 / <alpha-value>)',
          200: 'rgb(191 219 254 / <alpha-value>)',
          300: 'rgb(147 197 253 / <alpha-value>)',
          400: 'rgb(96 165 250 / <alpha-value>)',
          500: 'rgb(var(--cn-accent-rgb, 59 130 246) / <alpha-value>)',
          600: 'rgb(var(--cn-accent-hover-rgb, 37 99 235) / <alpha-value>)',
          700: 'rgb(var(--cn-accent-hover-rgb, 29 78 216) / <alpha-value>)',
          800: 'rgb(30 64 175 / <alpha-value>)',
          900: 'rgb(30 58 138 / <alpha-value>)',
          950: 'rgb(23 37 84 / <alpha-value>)',
        },
      },
    },
  },
  plugins: [],
}
