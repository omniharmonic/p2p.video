import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

async function load(rel) {
  const source = await readFile(new URL(rel, import.meta.url), 'utf8');
  const js = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText;
  return import(`data:text/javascript;base64,${Buffer.from(js).toString('base64')}`);
}
const { linkifySegments, safeHttpUrl } = await load('../src/lib/linkify.ts');
const { matchShortcut, isTypingTarget, shortcutLabels } = await load('../src/lib/shortcuts.ts');
const { handQueue, denialMessage, mergeChat, roleLabel, nextAdmission } = await load('../src/lib/callstate.ts');
const { AllowedReactions, ChatMaxChars } = await load('../src/lib/protocol.ts');

test('linkify turns only http(s) URLs into links and leaves markup as text', () => {
  const segs = linkifySegments('<img src=x onerror=alert(1)> see https://example.com/a?b=1, and javascript:alert(1)');
  assert.deepEqual(segs, [
    { kind: 'text', text: '<img src=x onerror=alert(1)> see ' },
    { kind: 'link', text: 'https://example.com/a?b=1', href: 'https://example.com/a?b=1' },
    { kind: 'text', text: ', and javascript:alert(1)' },
  ]);
  assert.equal(linkifySegments('no links here').length, 1);
  assert.deepEqual(linkifySegments(''), []);
});

test('linkify trims sentence punctuation but keeps balanced parentheses', () => {
  const [, link, tail] = linkifySegments('Read (https://en.wikipedia.org/wiki/Mix_(audio)).');
  assert.equal(link.href, 'https://en.wikipedia.org/wiki/Mix_(audio)');
  assert.equal(tail.text, ').');
  const segs = linkifySegments('Go to http://host.ts.net:8443/x!');
  assert.equal(segs[1].href, 'http://host.ts.net:8443/x');
  assert.equal(segs[2].text, '!');
});

test('linkify never produces non-http schemes or quote-breaking hrefs', () => {
  for (const evil of ['javascript:alert(1)', 'data:text/html,<b>x</b>', 'JaVaScRiPt:alert(1)', 'vbscript:x', 'file:///etc/passwd']) {
    assert.ok(linkifySegments(evil).every(s => s.kind === 'text'), evil);
  }
  assert.equal(safeHttpUrl('ftp://example.com'), null);
  assert.equal(safeHttpUrl('https://'), null);
  const [, link] = linkifySegments('x https://example.com/"onmouseover="alert(1)');
  assert.ok(!link.href.includes('"'));
});

test('Meet shortcuts match per platform and ignore typing targets', () => {
  const key = (k, mods = {}) => ({ key: k, code: /^[a-z]$/.test(k) ? `Key${k.toUpperCase()}` : undefined, metaKey: false, ctrlKey: false, altKey: false, shiftKey: false, ...mods });
  assert.equal(matchShortcut(key('d', { metaKey: true }), true), 'toggle-mic');
  assert.equal(matchShortcut(key('e', { metaKey: true }), true), 'toggle-cam');
  assert.equal(matchShortcut(key('d', { ctrlKey: true }), false), 'toggle-mic');
  assert.equal(matchShortcut(key('e', { ctrlKey: true }), false), 'toggle-cam');
  assert.equal(matchShortcut(key('d', { ctrlKey: true }), true), null, 'Ctrl+D on mac is not the mic shortcut');
  assert.equal(matchShortcut(key('h', { metaKey: true, ctrlKey: true }), true), 'raise-hand');
  assert.equal(matchShortcut(key('h', { ctrlKey: true, altKey: true }), false), 'raise-hand');
  assert.equal(matchShortcut(key('h', { metaKey: true }), true), null);
  assert.equal(matchShortcut(key('?', { shiftKey: true }), true), 'help');
  assert.equal(matchShortcut(key('d', { metaKey: true, shiftKey: true }), true), null);
  assert.equal(matchShortcut({ ...key('d', { metaKey: true }), repeat: true }, true), null);
  // Non-Latin layouts: physical key code still matches.
  assert.equal(matchShortcut({ key: 'в', code: 'KeyD', metaKey: true, ctrlKey: false, altKey: false, shiftKey: false }, true), 'toggle-mic');
  for (const target of [{ tagName: 'TEXTAREA' }, { tagName: 'INPUT', type: 'text' }, { tagName: 'INPUT' }, { tagName: 'DIV', isContentEditable: true }, { tagName: 'SELECT' }]) {
    assert.equal(isTypingTarget(target), true, JSON.stringify(target));
    assert.equal(matchShortcut(key('?', { shiftKey: true }), true, target), null);
    assert.equal(matchShortcut(key('d', { metaKey: true }), true, target), null);
  }
  assert.equal(isTypingTarget({ tagName: 'INPUT', type: 'checkbox' }), false);
  assert.equal(isTypingTarget({ tagName: 'BUTTON' }), false);
  assert.deepEqual(shortcutLabels(true).find(s => s.action === 'raise-hand').keys, ['Ctrl', '⌘', 'H']);
});

test('hand queue orders by raise time, then id', () => {
  const q = handQueue([
    { participantId: 'c', name: 'C', handRaised: true, handRaisedAt: 30 },
    { participantId: 'a', name: 'A', handRaised: false },
    { participantId: 'b', name: 'B', handRaised: true, handRaisedAt: 10 },
    { participantId: 'd', name: 'D', handRaised: true, handRaisedAt: 30 },
  ]);
  assert.deepEqual(q.map(p => p.participantId), ['b', 'c', 'd']);
});

test('chat history merge de-duplicates and keeps the newest window', () => {
  const m = (id, ts) => ({ id, ts });
  const merged = mergeChat([m('m1', 1), m('m2', 2)], [m('m2', 2), m('m3', 3), m('m10', 3)], 3);
  assert.deepEqual(merged.map(x => x.id), ['m2', 'm3', 'm10']);
});

test('denial messages are specific and role labels are human', () => {
  assert.match(denialMessage('removed').status, /Removed/);
  assert.match(denialMessage('room_locked').detail, /locked/);
  assert.match(denialMessage('busy').detail, /Wait a minute/); // SL-SEC-1 waiting-room cap
  assert.match(denialMessage(undefined).status, /expired or declined/);
  assert.equal(roleLabel('host'), 'Host');
  assert.equal(roleLabel('guest'), 'Guest');
  assert.equal(roleLabel('producer'), 'Producer');
});

test('protocol.ts limits mirror protocol.go', async () => {
  const go = await readFile(new URL('../../host/internal/signaling/protocol.go', import.meta.url), 'utf8');
  const goReactions = go.match(/AllowedReactions = \[\]string\{([^}]*)\}/)[1].match(/"([^"]+)"/g).map(s => JSON.parse(s));
  assert.deepEqual([...AllowedReactions], goReactions);
  assert.equal(ChatMaxChars, Number(go.match(/ChatMaxRunes\s+=\s+(\d+)/)[1]));
  for (const type of ['chat', 'chat_history', 'hand', 'reaction', 'roster', 'room_state', 'mute_request', 'remove', 'room_lock', 'action_error']) {
    assert.ok(go.includes(`= "${type}"`), type);
  }
});

test('admission banner: host sees a waiting producer first; producer sees guests only [SL-QA-1]', () => {
  const ps = [
    { participantId: 'h', status: 'admitted', role: 'host' },
    { participantId: 'a', status: 'pending', role: 'guest' },
    { participantId: 'b', status: 'pending', role: 'guest' },
    { participantId: 'p', status: 'pending', role: 'producer' },
  ];
  assert.equal(nextAdmission(ps, 'host')?.participantId, 'p', 'only the host can admit the producer');
  assert.equal(nextAdmission(ps, 'producer')?.participantId, 'a', 'arrival order among guests');
  assert.equal(nextAdmission(ps.filter(p => p.role !== 'producer'), 'host')?.participantId, 'a');
  assert.equal(nextAdmission([{ participantId: 'p', status: 'pending', role: 'producer' }], 'producer'), undefined);
  assert.equal(nextAdmission([], 'host'), undefined);
});
