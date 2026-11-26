import { StoryAudio } from './audio.js';

const types = Object.freeze({
  HELLO: 'mock.hello', LISTEN: 'mock.listen', START: 'mock.record.start',
  FINISH: 'mock.record.finish', HISTORY: 'mock.history', LEAVE: 'mock.leave',
  END: 'mock.end', RESULT: 'mock.playback.result', READY: 'mock.ready',
  BUDDY: 'mock.buddy', STATE: 'lobby.state', PLAY: 'playback.start',
  PLAY_END: 'playback.end', PLAY_STOP: 'playback.stop', ERROR: 'error',
  NOTICE: 'lobby.notice', ACCEPTED: 'turn.accepted',
});
const element = id => document.getElementById(id);
const requested = new URLSearchParams(location.search).get('box')?.toUpperCase();
const name = '伙伴 ' + (['A', 'B', 'C'].includes(requested) ? requested : 'A');
const url = new URL('/mock-linx/ws', location.href);
url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
const socket = new WebSocket(url);
socket.binaryType = 'arraybuffer';
let connected = false;
let view = { phase: 'idle', members: [], story: [] };
let playback = null;
let starting = false;
let stopping = false;

function send(type, values = {}) {
  if (connected) socket.send(JSON.stringify({ type, ...values }));
}
function error(text = '') {
  element('error').textContent = text;
  element('error').hidden = !text;
}
function finished(result) {
  if (!playback) return;
  const id = playback.id;
  playback = null;
  if (result !== 'finished') audio.flush();
  send(types.RESULT, { playback_id: id, result });
  controls();
}
const audio = new StoryAudio({
  onFrame(frame) { if (view.recording && !stopping && connected) socket.send(frame); },
  onActivity() {},
  onDone() { finished('finished'); },
  onError(text) { finished('failed'); error(text); },
});
function controls() {
  const inside = view.phase === 'waiting' || view.phase === 'playing';
  element('send').disabled = !connected;
  element('record').disabled = !connected || starting || stopping || !(view.can_speak || view.recording);
  element('record').textContent = view.recording ? '说完了' : '开始接故事';
  element('history').disabled = !connected || view.phase !== 'waiting' || view.members.length !== 1;
  element('leave').disabled = !connected || !inside;
  element('end').disabled = !connected || !view.can_end;
}
function render(state) {
  const wasRecording = view.recording;
  view = state;
  if (wasRecording && !state.recording && audio.stream && !stopping) {
    void audio.stopMic().catch(err => error(err.message));
  }
  element('room').textContent = state.topic || '游戏大厅';
  element('count').textContent = state.members.length + ' 人';
  if (state.phase === 'waiting') element('status').textContent = '正在匹配：三人到齐开始，三分钟后两人也可开始。';
  else if (state.phase === 'ended') element('status').textContent = '这一局结束了。';
  else if (state.recording) element('status').textContent = '正在录音，说完后点「说完了」。';
  else if (state.waiting_on) element('status').textContent = '等待 ' + state.waiting_on + ' 听完上一句。';
  else if (state.can_speak) element('status').textContent = '轮到你接一句。';
  else if (state.phase === 'playing') element('status').textContent = '轮到 ' + state.speaker + '。';
  else element('status').textContent = '先对模拟伙伴说「我要玩游戏」。';
  element('members').replaceChildren(...state.members.map(member => {
    const li = document.createElement('li');
    li.textContent = member.name + (member.self ? '（你）' : '');
    return li;
  }));
  element('lines').replaceChildren(...state.story.map(line => {
    const li = document.createElement('li');
    li.textContent = line.author + (line.historical ? ' · 历史原声' : '') + '：' + (line.text || '原声片段');
    return li;
  }));
  controls();
}
element('name').textContent = name;
socket.addEventListener('open', () => {
  connected = true;
  element('connection').textContent = name + ' · 模拟灵矽已连接';
  send(types.HELLO, { name });
  controls();
});
socket.addEventListener('close', () => {
  connected = false;
  element('connection').textContent = name + ' · 已断开';
  void audio.close().catch(err => error(err.message));
  controls();
});
socket.addEventListener('message', ({ data }) => {
  if (data instanceof ArrayBuffer) {
    if (playback) {
      try { audio.push(data); } catch (err) { finished('failed'); error(err.message); }
    }
    return;
  }
  const event = JSON.parse(data);
  switch (event.type) {
    case types.STATE: render(event.state); break;
    case types.BUDDY:
    case types.NOTICE:
    case types.ACCEPTED: element('buddy').textContent = event.text; break;
    case types.PLAY:
      if (playback) finished('stopped');
      audio.flush();
      playback = event.play;
      element('buddy').textContent = playback.historical ? '正在听以前伙伴的原声。' : '正在听 ' + playback.author + '。';
      controls();
      break;
    case types.PLAY_END:
      if (playback?.id === event.play.id) {
        audio.finish().catch(err => { finished('failed'); error(err.message); });
      }
      break;
    case types.PLAY_STOP:
      if (playback?.id === event.play.id) finished('stopped');
      break;
    case types.ERROR:
      if (audio.stream) void audio.stopMic().catch(err => error(err.message));
      error(event.text);
      break;
  }
});
element('talk-form').addEventListener('submit', event => {
  event.preventDefault();
  void audio.unlock().then(() => {
    error();
    send(types.LISTEN, { text: element('talk').value, topic: element('topic').value });
    element('talk').value = '我要玩故事接龙';
  }).catch(err => error(err.message));
});
element('record').addEventListener('click', () => {
  if (view.recording) {
    stopping = true;
    controls();
    void audio.stopMic().then(() => send(types.FINISH, { share: element('share').checked }))
      .catch(err => error(err.message)).finally(() => { stopping = false; controls(); });
    return;
  }
  starting = true;
  controls();
  void audio.startMic().then(() => send(types.START))
    .catch(err => error(err.message)).finally(() => { starting = false; controls(); });
});
element('history').addEventListener('click', () => send(types.HISTORY));
element('leave').addEventListener('click', () => send(types.LEAVE));
element('end').addEventListener('click', () => send(types.END));
