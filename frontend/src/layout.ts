import { ref, watch } from 'vue'

export type LayoutMode = 'classic' | 'modern'

const LAYOUT_KEY = 'chirp.layout'

function readLayout(): LayoutMode {
  return localStorage.getItem(LAYOUT_KEY) === 'classic' ? 'classic' : 'modern'
}

const layout = ref<LayoutMode>(readLayout())

watch(layout, (value) => {
  localStorage.setItem(LAYOUT_KEY, value)
})

export function setLayout(mode: LayoutMode): void {
  layout.value = mode
}

export function useLayout() {
  return { layout, setLayout }
}
