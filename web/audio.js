const INPUT_RATE = 16000;
const DEFAULT_RATE = 16000;
const START_BUFFER = 0.07;
const FORMAT_PCM = 'pcm';
const FORMAT_OPUS = 'opus';
const VAD_RMS = 0.018;
const ACTIVITY_INTERVAL = 500;

export class StoryAudio {
  constructor({ onFrame, onActivity, onDone, onError }) {
    Object.assign(this, { onFrame, onActivity, onDone, onError });
    this.rate = DEFAULT_RATE;
    this.format = FORMAT_PCM;
    this.frameDuration = 20;
    this.sources = new Set();
    this.nextTime = 0;
    this.timestamp = 0;
    this.finishing = false;
    this.lastActivity = 0;
  }

  async unlock() {
    if (!this.output || this.output.state === 'closed') this.output = new AudioContext();
    await this.output.resume();
  }

  configure(params) {
    this.rate = params.sample_rate || DEFAULT_RATE;
    this.format = params.format || FORMAT_PCM;
    this.frameDuration = params.frame_duration || 20;
    if (this.format === FORMAT_OPUS) {
      if (typeof AudioDecoder === 'undefined') throw new Error('当前浏览器无法解码Opus，请使用新版Chrome或Edge。');
      this.decoder = new AudioDecoder({
        output: data => {
          try {
            const pcm = new Float32Array(data.numberOfFrames);
            data.copyTo(pcm, { planeIndex: 0, format: 'f32-planar' });
            this.schedule(pcm, data.sampleRate);
          } finally { data.close(); }
        },
        error: error => this.onError(`音频解码失败：${error.message}`),
      });
      this.decoder.configure({ codec: FORMAT_OPUS, sampleRate: this.rate, numberOfChannels: 1 });
    }
  }

  push(buffer) {
    this.finishing = false;
    if (this.format === FORMAT_OPUS) {
      this.decoder.decode(new EncodedAudioChunk({ type: 'key', timestamp: this.timestamp, data: buffer }));
      this.timestamp += this.frameDuration * 1000;
      return;
    }
    if (buffer.byteLength % 2 !== 0) throw new Error('收到的PCM音频长度不正确。');
    const view = new DataView(buffer);
    const pcm = new Float32Array(buffer.byteLength / 2);
    for (let i = 0; i < pcm.length; i++) pcm[i] = view.getInt16(i * 2, true) / 32768;
    this.schedule(pcm, this.rate);
  }

  schedule(pcm, rate) {
    const ctx = this.output;
    if (!ctx || ctx.state === 'closed') return;
    const buffer = ctx.createBuffer(1, pcm.length, rate);
    buffer.copyToChannel(pcm, 0);
    const source = ctx.createBufferSource();
    source.buffer = buffer;
    source.connect(ctx.destination);
    const at = Math.max(ctx.currentTime + START_BUFFER, this.nextTime);
    this.nextTime = at + buffer.duration;
    this.sources.add(source);
    source.onended = () => {
      this.sources.delete(source);
      if (this.finishing && this.sources.size === 0) this.onDone();
    };
    source.start(at);
  }

  async finish() {
    if (this.decoder?.state === 'configured') await this.decoder.flush();
    this.finishing = true;
    if (this.sources.size === 0) this.onDone();
  }

  flush() {
    this.finishing = false;
    for (const source of this.sources) {
      source.onended = null;
      source.stop();
    }
    this.sources.clear();
    this.nextTime = 0;
  }

  async startMic() {
    if (this.stream) return;
    if (!navigator.mediaDevices?.getUserMedia) throw new Error('麦克风需要在localhost或HTTPS页面中使用。');
    this.stream = await navigator.mediaDevices.getUserMedia({ audio: {
      channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true,
    } });
    try {
      this.input = new AudioContext({ sampleRate: INPUT_RATE });
      await this.input.audioWorklet.addModule('/capture-worklet.js');
      await this.input.resume();
      this.node = new AudioWorkletNode(this.input, 'story-capture');
      this.node.port.onmessage = ({ data: pcm }) => {
        const buffer = new ArrayBuffer(pcm.length * 2);
        const view = new DataView(buffer);
        let energy = 0;
        for (let i = 0; i < pcm.length; i++) {
          const value = Math.max(-1, Math.min(1, pcm[i]));
          view.setInt16(i * 2, Math.round(value * (value < 0 ? 32768 : 32767)), true);
          energy += value * value;
        }
        this.onFrame(buffer);
        const now = performance.now();
        if (Math.sqrt(energy / pcm.length) > VAD_RMS && now - this.lastActivity > ACTIVITY_INTERVAL) {
          this.lastActivity = now;
          this.onActivity();
        }
      };
      this.input.createMediaStreamSource(this.stream).connect(this.node);
      const mute = this.input.createGain();
      mute.gain.value = 0;
      this.node.connect(mute).connect(this.input.destination);
    } catch (error) {
      await this.stopMic();
      throw error;
    }
  }

  async stopMic() {
    this.stream?.getTracks().forEach(track => track.stop());
    this.stream = null;
    this.node?.disconnect();
    this.node = null;
    if (this.input && this.input.state !== 'closed') await this.input.close();
    this.input = null;
  }

  async close() {
    await this.stopMic();
    this.flush();
    if (this.decoder && this.decoder.state !== 'closed') this.decoder.close();
    this.decoder = null;
    if (this.output && this.output.state !== 'closed') await this.output.close();
    this.output = null;
  }
}
