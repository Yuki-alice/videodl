<script>
  import Hls from 'hls.js'
  import { previewUrl } from '../lib/store.js'

  let videoEl
  let hls = null
  let current = ''

  $: if ($previewUrl && $previewUrl !== current) {
    current = $previewUrl
    play()
  }

  function play() {
    if (!videoEl || !current) return
    if (hls) {
      hls.destroy()
      hls = null
    }
    if (current.includes('.m3u8') && Hls.isSupported()) {
      hls = new Hls()
      hls.loadSource(current)
      hls.attachMedia(videoEl)
    } else {
      videoEl.src = current
    }
    videoEl.play().catch(() => {})
  }

  function close() {
    if (hls) {
      hls.destroy()
      hls = null
    }
    if (videoEl) videoEl.pause()
    current = ''
    previewUrl.set('')
  }
</script>

{#if $previewUrl}
  <div class="overlay" on:click={close} on:keydown={(e) => e.key === 'Escape' && close()} role="presentation">
    <!-- svelte-ignore a11y-click-events-have-key-events -->
    <div class="player card" role="dialog" aria-label="视频预览" on:click|stopPropagation>
      <div class="player-head">
        <strong>预览</strong>
        <button class="btn small" on:click={close}>关闭</button>
      </div>
      <!-- svelte-ignore a11y-media-has-caption -->
      <video bind:this={videoEl} controls></video>
      <div class="muted">{$previewUrl.slice(0, 100)}</div>
    </div>
  </div>
{/if}

<style>
  .overlay {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.5);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 50;
  }
  .player {
    width: min(720px, 92vw);
    padding: 12px;
  }
  .player-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 8px;
  }
  video {
    width: 100%;
    max-height: 60vh;
    background: #000;
    border-radius: 8px;
  }
</style>
