<script>
  import {
    tasks, pauseTask, resumeTask, removeTask, reveal,
    statusLabel, statusClass, pct, shortUrl, fileNameOf
  } from '../lib/store.js'

  $: list = [...$tasks].sort((a, b) => (a.id < b.id ? 1 : -1))
</script>

{#if list.length}
  <div class="grid">
    {#each list as t (t.id)}
      <div class="task card">
        <div class="cand-top">
          <span class="pill {statusClass(t)}">{statusLabel(t)}</span>
          <span class="cand-label">{fileNameOf(t)}</span>
          <span class="pct">{pct(t)}%</span>
        </div>
        <div class="bar"><div class="fill {statusClass(t)}" style="width:{pct(t)}%"></div></div>
        <div class="cand-url" title={t.mediaUrl}>
          {t.total > 10000 ? `${(t.done / 1048576).toFixed(1)}MB / ${(t.total / 1048576).toFixed(1)}MB` : `${t.done}/${t.total} 分片`} · {shortUrl(t.mediaUrl, 60)}
        </div>
        {#if t.status === 'done' && t.output}
          <div class="cand-url ok" title={t.output}>{t.output}</div>
        {/if}
        {#if t.status === 'failed' && t.error}
          <div class="cand-url err" title={t.error}>{t.error.slice(0, 160)}</div>
        {/if}
        <div class="cand-actions">
          {#if t.status === 'downloading'}
            <button class="btn small" on:click={() => pauseTask(t.id)}>暂停</button>
          {/if}
          {#if t.status === 'paused' || t.status === 'failed' || t.status === 'pausing'}
            <button class="btn small primary" on:click={() => resumeTask(t.id)}>继续</button>
          {/if}
          {#if t.status === 'done' && t.output}
            <button class="btn small" on:click={() => reveal(t.output)}>定位文件</button>
          {/if}
          {#if t.status !== 'downloading' && t.status !== 'pausing'}
            <button class="btn small danger-ghost" on:click={() => removeTask(t.id)}>删除</button>
          {/if}
        </div>
      </div>
    {/each}
  </div>
{:else}
  <div class="empty card">
    <div class="empty-title">暂无下载任务</div>
    <div class="muted">去「嗅探」页发现资源后点下载，任务会出现在这里；重启应用后未完成的任务可继续。</div>
  </div>
{/if}
