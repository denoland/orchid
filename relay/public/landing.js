// Scroll-driven hero→nav morph. `--scroll` ramps 0→1 over the first
// 30vh of scroll; the title shrinks/fades and the nav brand fades in
// against it. rAF coalesces scroll events.
{
  const body = document.body
  const update = () => {
    const range = window.innerHeight * 0.30
    const p = Math.min(1, Math.max(0, window.scrollY / range))
    body.style.setProperty('--scroll', p.toFixed(3))
  }
  let pending = false
  window.addEventListener('scroll', () => {
    if (pending) return
    pending = true
    requestAnimationFrame(() => { update(); pending = false })
  }, { passive: true })
  update()
}

// Mac recording — pause until in view, then play (camera zoom is baked
// into the file so no CSS animation to keep in sync).
const macShot = document.querySelector('.mac-shot')
if (macShot) {
  macShot.pause()
  macShot.loop = true
  const macObs = new IntersectionObserver((entries) => {
    for (const e of entries) {
      if (e.isIntersecting) macShot.play().catch(() => {})
      else macShot.pause()
    }
  }, { threshold: 0.4 })
  macObs.observe(macShot)
}

// Tiny scroll-reveal for the "how" steps.
const observer = new IntersectionObserver(
  (entries) => {
    for (const e of entries) {
      if (e.isIntersecting) {
        e.target.style.animation = 'rise 0.7s cubic-bezier(.2,.7,.3,1) both'
        observer.unobserve(e.target)
      }
    }
  },
  { threshold: 0.2 },
)
for (const el of document.querySelectorAll('.feature, .install h2, .install pre, .install .muted')) {
  el.style.opacity = '0'
  el.style.transform = 'translateY(20px)'
  observer.observe(el)
}
