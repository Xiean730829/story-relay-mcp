const TARGET_RATE = 16000;
const FRAME_SAMPLES = 320;

class StoryCapture extends AudioWorkletProcessor {
  constructor() {
    super();
    this.frame = new Float32Array(FRAME_SAMPLES);
    this.count = 0;
    this.phase = 0;
    this.sum = 0;
    this.samples = 0;
  }

  process(inputs) {
    const channel = inputs[0]?.[0];
    if (!channel) return true;
    const ratio = sampleRate / TARGET_RATE;
    for (const value of channel) {
      this.sum += value;
      this.samples++;
      this.phase++;
      if (this.phase < ratio) continue;
      this.frame[this.count++] = this.sum / this.samples;
      this.phase -= ratio;
      this.sum = 0;
      this.samples = 0;
      if (this.count === FRAME_SAMPLES) {
        const frame = this.frame;
        this.port.postMessage(frame, [frame.buffer]);
        this.frame = new Float32Array(FRAME_SAMPLES);
        this.count = 0;
      }
    }
    return true;
  }
}

registerProcessor('story-capture', StoryCapture);
