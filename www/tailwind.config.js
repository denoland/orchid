export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  darkMode: 'class',
  theme: {
    extend: {
      fontFamily: {
        mono: ['ui-monospace', 'SF Mono', 'Menlo', 'monospace'],
        serif: ['Cormorant Garamond', 'Iowan Old Style', 'Apple Garamond', 'Georgia', 'serif'],
      },
    },
  },
}
