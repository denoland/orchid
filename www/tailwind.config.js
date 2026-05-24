export default {
  content: [
    './index.html',
    './src/**/*.{ts,tsx}',
    // Whiteboard primitives ship Tailwind classes in their source —
    // scan them too so amber sticky notes / link gradients / etc.
    // don't get tree-shaken out of the bundle.
    '../lib/whiteboard/src/**/*.{ts,tsx}',
  ],
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
