<script>
  import { onMount, onDestroy } from 'svelte'
  import { EventsOn, EventsOff } from '../wailsjs/runtime/runtime.js'
  import {
    view, candidates, tasks, toasts, watching, activeTaskCount,
    initApp, refreshTasks, stopWatch, toast
  } from './lib/store.js'
  import Sniffer from './components/Sniffer.svelte'
  import TaskList from './components/TaskList.svelte'
  import SettingsPanel from './components/SettingsPanel.svelte'
  import Player from './components/Player.svelte'

  let refreshTimer = null

  onMount(async () => {
    await initApp()
    EventsOn('task://progress', (p) => {
      tasks.update((l) =>
        l.map((t) => (t.id === p.id ? { ...t, done: p.done, total: p.total, status: 'downloading' } : t))
      )
    })
    EventsOn('task://done', (p) => {
      if (p.status === 'done') toast('下载完成', 'ok')
      else if (p.status === 'failed') toast('下载失败：' + (p.error || ''), 'error', 6000)
      refreshTasks()
    })
    refreshTimer = setInterval(refreshTasks, 5000)
  })
  // 必须在组件初始化阶段同步注册，不能放在 async 回调的 await 之后（Svelte 5 会抛 effect_orphan）
  onDestroy(() => {
    if (refreshTimer) clearInterval(refreshTimer)
    EventsOff('task://progress')
    EventsOff('task://done')
    stopWatch()
  })
</script>

<div class="shell">
  <aside>
    <div class="brand">VideoDL</div>
    <nav>
      <button class:active={$view === 'sniffer'} on:click={() => view.set('sniffer')}>
        🔍 嗅探 <span class="badge">{$candidates.length || ''}</span>
      </button>
      <button class:active={$view === 'tasks'} on:click={() => view.set('tasks')}>
        ⬇ 任务
        {#if $activeTaskCount}<span class="badge live">{$activeTaskCount}</span>{/if}
      </button>
      <button class:active={$view === 'settings'} on:click={() => view.set('settings')}>⚙ 设置</button>
    </nav>
    <div class="sidefoot muted">
      {#if $watching}<div class="ok">● 浏览嗅探中</div>{/if}
      <div>明文 HLS / DASH / 直链</div>
    </div>
  </aside>

  <main>
    {#if $view === 'sniffer'}
      <Sniffer />
    {:else if $view === 'tasks'}
      <TaskList />
    {:else}
      <SettingsPanel />
    {/if}
  </main>

  <Player />

  <div class="toasts">
    {#each $toasts as t (t.id)}
      <div class="toast {t.kind}">{t.text}</div>
    {/each}
  </div>
</div>

<style>
  .shell {
    display: flex;
    height: 100vh;
    text-align: left;
  }
  aside {
    width: 180px;
    flex: none;
    border-right: 1px solid var(--border);
    padding: 16px 12px;
    display: flex;
    flex-direction: column;
    gap: 12px;
    background: var(--side);
  }
  .brand {
    font-weight: 800;
    font-size: 18px;
    padding: 0 8px;
  }
  nav {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  nav button {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 9px 10px;
    border: none;
    border-radius: 8px;
    background: transparent;
    color: inherit;
    font-size: 14px;
    cursor: pointer;
    text-align: left;
  }
  nav button:hover {
    background: var(--hover);
  }
  nav button.active {
    background: var(--accent-soft);
    color: var(--accent);
    font-weight: 600;
  }
  .badge {
    margin-left: auto;
    font-size: 11px;
    background: var(--hover);
    border-radius: 10px;
    padding: 1px 8px;
  }
  .badge.live {
    background: var(--accent);
    color: #fff;
  }
  .sidefoot {
    margin-top: auto;
    font-size: 12px;
    padding: 0 8px;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  main {
    flex: 1;
    overflow-y: auto;
    padding: 20px 22px 60px;
  }
  .toasts {
    position: fixed;
    right: 16px;
    bottom: 16px;
    display: flex;
    flex-direction: column;
    gap: 8px;
    z-index: 60;
    max-width: 360px;
  }
  .toast {
    background: var(--card);
    border: 1px solid var(--border);
    border-radius: 10px;
    padding: 10px 14px;
    font-size: 13px;
    box-shadow: 0 6px 24px rgba(0, 0, 0, 0.12);
  }
  .toast.ok { border-color: #22c55e; }
  .toast.warn { border-color: #f59e0b; }
  .toast.error { border-color: #ef4444; }
</style>
