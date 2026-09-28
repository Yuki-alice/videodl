<script>
  import { config, ffmpegPath, saveConfig } from '../lib/store.js'
</script>

<div class="card settings">
  {#if !$ffmpegPath}
    <div class="warnbox">未检测到 ffmpeg（m3u8 合并与 DASH 混流必需）：<code>brew install ffmpeg</code></div>
  {:else}
    <div class="okbox">ffmpeg：{$ffmpegPath}</div>
  {/if}
  <label>下载并发（1–64）
    <input type="number" min="1" max="64" bind:value={$config.workers} />
  </label>
  <label>全局限速 KB/s（0 = 不限）
    <input type="number" min="0" bind:value={$config.speedLimitKBs} />
  </label>
  <label>下载目录（空 = 系统下载文件夹下的 videodl）
    <input bind:value={$config.outDir} placeholder="~/Downloads/videodl" />
  </label>
  <div>
    <button class="btn primary" on:click={saveConfig}>保存设置</button>
    <span class="muted">新创建的任务生效</span>
  </div>
  <div class="muted">任务与分片保存在 ~/.videodl/tasks，重启可续；配置保存在 ~/.videodl/config.json。</div>
</div>

<style>
  .settings {
    display: flex;
    flex-direction: column;
    gap: 12px;
    max-width: 560px;
  }
  label {
    display: flex;
    flex-direction: column;
    gap: 6px;
    font-size: 13px;
  }
  .warnbox {
    background: #fef2f2;
    color: #991b1b;
    border: 1px solid #fecaca;
    border-radius: 8px;
    padding: 10px 12px;
    font-size: 13px;
  }
  .okbox {
    background: #f0fdf4;
    color: #166534;
    border: 1px solid #bbf7d0;
    border-radius: 8px;
    padding: 10px 12px;
    font-size: 13px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  @media (prefers-color-scheme: dark) {
    .warnbox { background: #3f1d1d; color: #fca5a5; border-color: #7f2d2d; }
    .okbox { background: #12351f; color: #86efac; border-color: #1f5c33; }
  }
</style>
