import { StoryAudio } from './audio.js';

const TYPE = Object.freeze({
  CONNECT: 'connect', TEXT: 'text', REPLACE: 'replace', END: 'end', PLAYBACK: 'playback.done', VOICE: 'voice.activity',
  PHASE: 'phase', READY: 'ready', ERROR: 'error', STORY: 'story', TRACE: 'trace', PLATFORM: 'platform',
});
const STATUS = Object.freeze({ READY: 'ready', PLAYING: 'playing', ENDED: 'ended' });
const PLATFORM = Object.freeze({ HELLO: 'hello', TTS: 'tts', STT: 'stt', START: 'start', STOP: 'stop', SENTENCE: 'sentence_start' });
const STORAGE = Object.freeze({ DEVICE: 'story-relay-device', AGENT: 'story-relay-agent' });
const $ = id => document.getElementById(id);
let socket = null;
let audio = null;
let ready = false;
let storyStatus = STATUS.READY;
let toolCalls = 0;
let userDisconnected = false;
let proxyMode = false;

function showError(message) {
  $('error').textContent = message;
  $('error').hidden = false;
}

function send(type, text = '') {
  if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type, text }));
}

function updateControls() {
  for (const id of ['replace', 'sentence', 'send']) $(id).disabled = !ready;
  $('mic').disabled = !ready || storyStatus === STATUS.ENDED;
  $('end').disabled = !ready || storyStatus === STATUS.ENDED;
  $('connect').disabled = socket !== null;
  $('disconnect').hidden = socket === null;
  $('api-key').disabled = socket !== null;
  $('create-agent').disabled = socket !== null || proxyMode;
  $('agent-id').disabled = socket !== null || proxyMode || $('create-agent').checked;
  $('device-id').disabled = socket !== null;
  $('first-speaker').disabled = socket !== null;
  $('silence').disabled = socket !== null;
}

function micLabel() {
  const running = Boolean(audio?.stream);
  $('mic').textContent = running ? '麦克风已开启 · 点击暂停' : '开启麦克风';
  $('mic').classList.toggle('recording', running);
}

function makeDeviceID() {
  const bytes = crypto.getRandomValues(new Uint8Array(6));
  bytes[0] = (bytes[0] | 2) & 254;
  return [...bytes].map(n => n.toString(16).padStart(2, '0')).join(':');
}

function rememberIdentity(data) {
  if (!data?.agent_id) return;
  $('agent-id').value = data.agent_id;
  $('create-agent').checked = false;
  localStorage.setItem(STORAGE.AGENT, data.agent_id);
  localStorage.setItem(STORAGE.DEVICE, data.device_id);
}

function renderStory(story) {
  storyStatus = story.status;
  $('story-title').textContent = story.title || '等待你的第一句';
  $('story-count').textContent = `${story.lines.length} 句`;
  const fragment = document.createDocumentFragment();
  for (const line of story.lines) {
    const child = line.role === 'child';
    const article = document.createElement('article');
    article.className = `story-line ${child ? 'child' : 'ai'}`;
    const avatar = document.createElement('div');
    avatar.className = 'avatar';
    avatar.textContent = child ? '你' : '✦';
    const content = document.createElement('div');
    const label = document.createElement('small');
    label.textContent = child ? '你的奇思妙想' : '小星接了一句';
    const text = document.createElement('p');
    text.textContent = line.text;
    content.append(label, text);
    article.append(avatar, content);
    fragment.append(article);
  }
  $('story').replaceChildren(fragment);
  $('story').scrollTop = $('story').scrollHeight;
  if (story.status === STATUS.ENDED) {
    const reasons = { silence: '等了一会儿没有新的一句，故事先停在这里。', child_stop: '故事已结束，下次再一起编。', ai_finish: '故事有了一个温暖的结局。' };
    $('hint').textContent = `${reasons[story.reason] || '故事已结束。'} 点“换个故事”可以重新开始。`;
    if (audio) audio.stopMic().then(micLabel).catch(error => showError(error.message));
  }
  updateControls();
}

function renderTrace(trace) {
  if ($('trace').querySelector('.muted')) $('trace').replaceChildren();
  const item = document.createElement('li');
  if (!trace.ok) item.className = 'failed';
  const method = document.createElement('strong');
  method.textContent = trace.tool || trace.method;
  const detail = document.createElement('span');
  detail.textContent = `${trace.ok ? '成功' : '未完成'} · 请求 ${trace.id}${trace.detail ? ` · ${trace.detail}` : ''}`;
  item.append(method, detail);
  $('trace').prepend(item);
  while ($('trace').children.length > 60) $('trace').lastElementChild.remove();
  if (trace.method === 'tools/call') toolCalls++;
  $('tool-count').textContent = `${toolCalls} 次工具调用`;
}

async function onPlatform(message) {
  switch (message.type) {
    case PLATFORM.HELLO:
      audio.configure(message.audio_params);
      break;
    case PLATFORM.STT:
      if (message.text && storyStatus !== STATUS.ENDED) $('hint').textContent = `听到你说：“${message.text}”`;
      break;
    case PLATFORM.TTS:
      switch (message.state) {
        case PLATFORM.START:
          if (storyStatus !== STATUS.ENDED) $('hint').textContent = '小星正在接故事，听一听它的奇思妙想…';
          break;
        case PLATFORM.SENTENCE:
          if (message.text && storyStatus !== STATUS.ENDED) $('hint').textContent = `小星：“${message.text}”`;
          break;
        case PLATFORM.STOP:
          if (message.is_aborted) audio.flush();
          await audio.finish();
          break;
      }
      break;
  }
}

async function disconnect() {
  userDisconnected = true;
  const previous = socket;
  socket = null;
  ready = false;
  previous?.close();
  const previousAudio = audio;
  audio = null;
  if (previousAudio) await previousAudio.close();
  $('connection').textContent = '已断开';
  $('connection').classList.remove('connected');
  updateControls();
  micLabel();
}

$('connect-form').addEventListener('submit', async event => {
  event.preventDefault();
  if (socket) return;
  $('error').hidden = true;
  const options = {
    api_key: $('api-key').value.trim(), agent_id: $('agent-id').value.trim(),
    device_id: $('device-id').value.trim(), create_agent: $('create-agent').checked,
    first_speaker: $('first-speaker').value, silence_seconds: Number($('silence').value),
  };
  try {
    audio = new StoryAudio({
      onFrame: frame => { if (ready && socket?.readyState === WebSocket.OPEN && socket.bufferedAmount < 65536) socket.send(frame); },
      onActivity: () => send(TYPE.VOICE),
      onDone: () => {
        send(TYPE.PLAYBACK);
        if (storyStatus !== STATUS.ENDED) $('hint').textContent = audio?.stream ? '轮到你啦，说出下一句故事。' : '轮到你啦，开启麦克风或打字接一句。';
      },
      onError: showError,
    });
    await audio.unlock();
    userDisconnected = false;
    storyStatus = STATUS.READY;
    toolCalls = 0;
    $('trace').replaceChildren();
    $('tool-count').textContent = '等待 MCP';
    $('connection').textContent = '正在连接…';
    const current = new WebSocket(`${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/ws`);
    socket = current;
    current.binaryType = 'arraybuffer';
    updateControls();
    current.onopen = () => {
      current.send(JSON.stringify({ type: TYPE.CONNECT, options }));
      options.api_key = '';
    };
    current.onmessage = async ({ data }) => {
      if (socket !== current) return;
      try {
        if (data instanceof ArrayBuffer) { audio.push(data); return; }
        const message = JSON.parse(data);
        switch (message.type) {
          case TYPE.PHASE: $('hint').textContent = message.text; rememberIdentity(message.data); break;
          case TYPE.READY:
            ready = true;
            $('api-key').value = '';
            $('connection').textContent = '已连接灵矽';
            $('connection').classList.add('connected');
            $('hint').textContent = message.text;
            updateControls();
            break;
          case TYPE.ERROR: showError(message.text); break;
          case TYPE.STORY: renderStory(message.data); break;
          case TYPE.TRACE: renderTrace(message.data); break;
          case TYPE.PLATFORM: await onPlatform(message.data); break;
        }
      } catch (error) { showError(error.message); }
    };
    current.onerror = () => showError('本地服务连接失败，请确认故事接龙程序正在运行。');
    current.onclose = async () => {
      if (socket !== current) return;
      socket = null;
      ready = false;
      const previousAudio = audio;
      audio = null;
      if (previousAudio) await previousAudio.close();
      $('connection').textContent = '连接已断开';
      $('connection').classList.remove('connected');
      if (!userDisconnected && $('error').hidden) showError('连接已断开，请重新连接。');
      updateControls();
      micLabel();
    };
  } catch (error) {
    showError(error.message);
    await disconnect();
  }
});

$('create-agent').addEventListener('change', updateControls);
$('disconnect').addEventListener('click', () => disconnect().catch(error => showError(error.message)));
$('mic').addEventListener('click', async () => {
  if (!ready || !audio) return;
  try {
    if (audio.stream) await audio.stopMic();
    else { await audio.unlock(); await audio.startMic(); }
    micLabel();
    if (audio.stream) $('hint').textContent = '麦克风已开启，你可以接下一句，也可以说“换个故事”或“结束故事”。';
  } catch (error) { showError(`麦克风无法开启：${error.message}。仍然可以打字接龙。`); }
});
$('replace').addEventListener('click', () => {
  storyStatus = STATUS.READY;
  audio?.flush();
  send(TYPE.REPLACE);
  $('hint').textContent = '小星正在准备一个新故事…';
  updateControls();
});
$('end').addEventListener('click', () => { audio?.flush(); send(TYPE.END); });
$('text-form').addEventListener('submit', event => {
  event.preventDefault();
  const text = $('sentence').value.trim();
  if (!ready || !text) return;
  audio?.flush();
  send(TYPE.TEXT, text);
  $('sentence').value = '';
  $('hint').textContent = '小星收到你的一句了，正在想象接下来的故事…';
});

async function init() {
  $('device-id').value = localStorage.getItem(STORAGE.DEVICE) || makeDeviceID();
  localStorage.setItem(STORAGE.DEVICE, $('device-id').value);
  const response = await fetch('/api/status', { cache: 'no-store' });
  const { data } = await response.json();
  proxyMode = data.mcp_mode === 'proxy';
  if (data.deployment) {
    const deployment = data.deployment;
    $('account-info').hidden = false;
    $('account-name').textContent = deployment.account;
    $('deployed-agent').textContent = deployment.agent_id;
    $('deployed-server').textContent = deployment.server_id;
    $('deployed-mcp').textContent = deployment.mcp_id;
    $('proxy-expiry').textContent = new Date(deployment.expires_at).toLocaleString('zh-CN');
    $('proxy-status').textContent = data.proxy_online ? '已连接' : '重连中，请稍后刷新';
    $('tool-count').textContent = data.proxy_online ? 'Proxy 已连接' : 'Proxy 重连中';
  }
  const agent = data.agent_id || localStorage.getItem(STORAGE.AGENT);
  if (agent) { $('agent-id').value = agent; $('create-agent').checked = false; }
  if (data.has_key) $('api-key').placeholder = '已从本机环境变量读取，可直接连接';
  updateControls();
}
init().catch(error => showError(`初始化失败：${error.message}`));
