import { writable, derived, get } from 'svelte/store'
import {
  Probe, DeepProbe, GetVariants, StartDownload, PauseTask, ResumeTask,
  DeleteTask, RevealOutput, ListTasks, CheckFFmpeg,
  StartWatch, PollWatch, StopWatch, GetConfig, SetConfig
} from '../../wailsjs/go/main/App.js'

// ---------- 状态 ----------
export const pageUrl = writable('')
export const candidates = writable([])
export const tasks = writable([])
export const variants = writable({}) // {url: {loading, list, selected}}
export const toasts = writable([]) // {id, kind, text}
export const config = writable({ workers: 16, outDir: '', speedLimitKBs: 0 })
export const ffmpegPath = writable('')
export const previewUrl = writable('')
export const watching = writable(false)
export const watchTitle = writable('')
export const busyProbe = writable(false)
export const busyDeep = writable(false)
export const view = writable('sniffer') // sniffer | tasks | settings

export const activeTaskCount = derived(tasks, ($t) => $t.filter((t) => t.status === 'downloading').length)

let toastId = 0
export function toast(text, kind = 'info', ms = 3500) {
  const id = ++toastId
  toasts.update((l) => [...l, { id, text, kind }])
  setTimeout(() => toasts.update((l) => l.filter((t) => t.id !== id)), ms)
}

// ---------- 展示辅助 ----------
const kindMeta = {
  'm3u8-media': ['HLS', 'k-hls'],
  mpd: ['DASH', 'k-dash'],
  mp4: ['直链', 'k-mp4'],
  'page-video': ['页面视频', 'k-page'],
  blob: ['BLOB', 'k-blob']
}
export function kindLabel(c) {
  return kindMeta[c.kind]?.[0] ?? c.kind
}
export function kindClass(c) {
  return kindMeta[c.kind]?.[1] ?? 'k-other'
}
const statusMeta = {
  downloading: ['下载中', 's-run'],
  pausing: ['正在暂停', 's-pause'],
  paused: ['已暂停', 's-pause'],
  failed: ['失败', 's-fail'],
  done: ['完成', 's-done']
}
export function statusLabel(t) {
  return statusMeta[t.status]?.[0] ?? t.status
}
export function statusClass(t) {
  return statusMeta[t.status]?.[1] ?? 's-pause'
}
export function pct(t) {
  if (!t.total) return t.status === 'done' ? 100 : 0
  const v = t.total > 10000 ? t.done / t.total : t.done / Math.max(t.total, 1)
  return Math.min(100, Math.round(v * 100))
}
export function shortUrl(u, n = 80) {
  if (!u) return ''
  return u.length > n ? u.slice(0, n) + '…' : u
}
export function fileNameOf(t) {
  if (t.output) return t.output.split('/').pop()
  return shortUrl(t.mediaUrl, 60)
}
export function isPlaylist(c) {
  return (c.kind === 'm3u8-media' || c.kind === 'mpd') && c.kind !== 'blob'
}

// ---------- 后端动作 ----------
export async function refreshTasks() {
  try {
    tasks.set((await ListTasks()) ?? [])
  } catch {}
}

export async function initApp() {
  ffmpegPath.set(await CheckFFmpeg().catch(() => ''))
  try {
    const c = await GetConfig()
    config.set({ workers: c.workers, outDir: c.outDir, speedLimitKBs: c.speedLimitKBs || 0 })
  } catch {}
  await refreshTasks()
}

export async function doProbe() {
  const u = get(pageUrl).trim()
  if (!u) return
  busyProbe.set(true)
  try {
    candidates.set((await Probe(u)) ?? [])
    const n = get(candidates).length
    toast(n ? `轻嗅探发现 ${n} 个候选` : '轻嗅探没发现，试试重嗅探 / 浏览嗅探', n ? 'ok' : 'warn')
  } catch (e) {
    toast('嗅探失败：' + e, 'error')
  }
  busyProbe.set(false)
}

export async function doDeepProbe() {
  const u = get(pageUrl).trim()
  if (!u) return
  busyDeep.set(true)
  toast('重嗅探中（无头渲染约 15 秒）…')
  try {
    candidates.set((await DeepProbe(u, 12)) ?? [])
    const n = get(candidates).length
    toast(n ? `重嗅探发现 ${n} 个候选` : '没发现，可能用了 DRM 或私有协议', n ? 'ok' : 'warn')
  } catch (e) {
    toast('重嗅探失败：' + e, 'error')
  }
  busyDeep.set(false)
}

let watchId = ''
let pollTimer = null

export async function startWatch() {
  try {
    watchId = await StartWatch(get(pageUrl).trim())
    watching.set(true)
    watchTitle.set('')
    toast('受控浏览器已打开，在里面逛站自动发现（独立资料，需重新登录）', 'ok')
    pollTimer = setInterval(pollWatch, 3000)
  } catch (e) {
    toast('浏览嗅探启动失败：' + e, 'error')
  }
}

async function pollWatch() {
  if (!watchId) return
  try {
    const u = await PollWatch(watchId)
    if (u.title) watchTitle.set(u.title)
    const fresh = (u.candidates ?? []).filter(
      (c) => !get(candidates).some((x) => x.url === c.url && x.kind === c.kind)
    )
    if (fresh.length) {
      candidates.update((l) => [...l, ...fresh])
      toast(`新发现 ${fresh.length} 个视频（累计 ${get(candidates).length}）`, 'ok')
    }
  } catch (e) {
    stopWatch()
    toast('浏览会话结束：' + e, 'warn')
  }
}

export async function stopWatch() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
  if (watchId) {
    try {
      await StopWatch(watchId)
    } catch {}
    watchId = ''
  }
  watching.set(false)
}

export async function loadVariants(c) {
  variants.update((m) => ({ ...m, [c.url]: { loading: true, list: [] } }))
  try {
    const list = (await GetVariants(c.url, c.referer || get(pageUrl), c.cookie || '')) ?? []
    variants.update((m) => ({ ...m, [c.url]: { loading: false, list, selected: list[0]?.url } }))
  } catch (e) {
    toast('解析清晰度失败：' + e, 'error')
    variants.update((m) => ({ ...m, [c.url]: { loading: false, list: [] } }))
  }
}

export async function download(c) {
  const v = get(variants)[c.url]
  try {
    await StartDownload(get(pageUrl) || get(watchTitle), v?.selected || c.url, c.referer || get(pageUrl), c.cookie || '', '')
    toast('任务已创建，后台下载中', 'ok')
    view.set('tasks')
    await refreshTasks()
  } catch (e) {
    toast('创建任务失败：' + e, 'error')
  }
}

export async function downloadAll() {
  const list = get(candidates).filter((c) => c.kind !== 'blob')
  if (!list.length) {
    toast('没有可批量下载的候选', 'warn')
    return
  }
  let n = 0
  for (const c of list) {
    try {
      const v = get(variants)[c.url]
      await StartDownload(get(pageUrl) || get(watchTitle), v?.selected || c.url, c.referer || get(pageUrl), c.cookie || '', '')
      n++
    } catch (e) {
      toast(`批量下载中断于第 ${n + 1} 个：` + e, 'error')
      break
    }
  }
  toast(`已创建 ${n}/${list.length} 个下载任务`, 'ok')
  view.set('tasks')
  await refreshTasks()
}

export async function pauseTask(id) {
  try {
    await PauseTask(id)
    await refreshTasks()
  } catch (e) {
    toast('暂停失败：' + e, 'error')
  }
}

export async function resumeTask(id) {
  try {
    await ResumeTask(id)
    await refreshTasks()
  } catch (e) {
    toast('继续失败：' + e, 'error')
  }
}

export async function removeTask(id) {
  try {
    const delFiles = confirm('同时删除已下载的成片文件？（取消=只删任务记录）')
    await DeleteTask(id, delFiles)
    await refreshTasks()
  } catch (e) {
    toast('删除失败：' + e, 'error')
  }
}

export async function reveal(path) {
  try {
    await RevealOutput(path)
  } catch (e) {
    toast('定位失败：' + e, 'error')
  }
}

export function copyUrl(u) {
  navigator.clipboard?.writeText(u).then(
    () => toast('链接已复制', 'ok', 1500),
    () => toast('复制失败', 'error')
  )
}

export async function saveConfig() {
  const c = get(config)
  try {
    await SetConfig({ workers: Number(c.workers) || 16, outDir: c.outDir, speedLimitKBs: Number(c.speedLimitKBs) || 0 })
    toast('设置已保存（新任务生效）', 'ok')
  } catch (e) {
    toast('保存失败：' + e, 'error')
  }
}
