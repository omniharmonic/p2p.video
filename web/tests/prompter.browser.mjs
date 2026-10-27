// Teleprompter [SL-P5-1] against the real daemon with Chromium fake devices:
// the host writes a script, a guest sees it only once the host makes it
// visible to everyone, follow-host scroll sync moves the guest's reader with
// the host's, and the guest can't edit (no editor, and a forged edit over its
// own socket is refused by the hub). Build first; STUDIO_BINARY is the daemon.
//   cd web && STUDIO_TEST_PORT=19151 STUDIO_BINARY=$PWD/../bin/p2pvideod node tests/prompter.browser.mjs
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdtemp,mkdir,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs,launchURL,revealControl,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');

const data=await mkdtemp(path.join(tmpdir(),'studio-prompter-'));
const port=Number(process.env.STUDIO_TEST_PORT||19151),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
const errors=[];let browser;
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const MARKER='PROMPTER-PRIVATE-LINE-7731';
const SCRIPT=['# Cold open','Welcome back to The Long Take.','Today: why every podcast should record locally.','',
 '## Guest intro',`Our guest builds studios for a living. ${MARKER}`,
 ...Array.from({length:40},(_,i)=>`Line ${i+1}: keep your eyes near the lens while you read this.`),'','# Outro','Thanks for listening.'].join('\n');

// Captures the page's WebSockets so the test can send what a modified client could.
const captureSockets=()=>{const W=window.WebSocket;window.__sockets=[];window.WebSocket=class extends W{constructor(...a){super(...a);window.__sockets.push(this);}};};

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const open=async(name,viewport)=>{
  const context=await browser.newContext({permissions:['camera','microphone'],viewport});
  await context.addInitScript(captureSockets);
  const page=await context.newPage();page.setDefaultTimeout(45000);
  page.on('console',m=>{if(m.type()==='error'&&!/Failed to load resource/.test(m.text()))errors.push(`${name} console: ${m.text()}`);});
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
  return page;
 };
 const host=await open('Host',{width:1280,height:800});
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});

 const guest=await open('Guest',{width:390,height:844});
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await guest.locator('#name-input').fill('Gia');await guest.locator('#join-btn').click();
 await host.locator('#admit-btn').waitFor({state:'visible'});await host.locator('#admit-btn').click();
 await guest.locator('#studio-view').waitFor({state:'visible'});
 console.log('guest admitted');

 // The host writes the script (auto-saved), private by default.
 await (await revealControl(host,'#prompter-btn')).click();
 await host.locator('#prompter-editor.open').waitFor({state:'visible'});
 assert.equal(await host.locator('#prompter-visibility').inputValue(),'host','scripts start private to the host');
 await host.locator('#prompter-text').fill(SCRIPT);
 await host.waitForFunction(()=>/^Saved/.test(document.querySelector('#prompter-status').textContent));
 await screenshot('p5-prompter-editor',host);

 // Private: the guest has no teleprompter at all.
 await wait(500);
 assert.equal(await guest.locator('#prompter-btn').isHidden(),true,'guest sees a private script entry');
 assert.equal(await guest.evaluate(()=>document.querySelector('#prompter-view').textContent.includes('Cold open')),false,'private script reached the guest DOM');

 // Negative: a forged edit from the guest's own socket is refused by the hub.
 await guest.evaluate(()=>window.__sockets.filter(s=>s.readyState===1).at(-1).send(JSON.stringify({t:'prompter_edit',payload:{text:'hijacked',visibility:'everyone'}})));
 await guest.waitForFunction(()=>/Only the host or a producer can edit/.test(document.querySelector('#toast-container').textContent));
 await wait(300);
 assert.equal(await host.locator('#prompter-text').inputValue(),SCRIPT,'guest edit changed the script');
 assert.equal(await host.locator('#prompter-visibility').inputValue(),'host','guest changed visibility');

 // The host shares it with everyone; the guest gets a reader, never an editor.
 await host.locator('#prompter-visibility').selectOption('everyone');
 await guest.waitForFunction(()=>!document.querySelector('#prompter-btn').hidden);
 await (await revealControl(guest,'#prompter-btn')).click();
 await guest.locator('#prompter-reader').waitFor({state:'visible'});
 await guest.waitForFunction(()=>document.querySelector('#prompter-view').textContent.includes('Welcome back to The Long Take.'));
 assert.equal(await guest.locator('#prompter-editor.open').count(),0,'guest opened the editor');
 assert.equal(await guest.locator('#prompter-edit').isHidden(),true,'guest has an Edit button');
 assert.equal(await guest.locator('#prompter-view h2.prompter-h1').first().textContent(),'Cold open');
 // Untrusted text is text: the host's markup-looking line is not parsed as HTML.
 await host.locator('#prompter-text').fill(SCRIPT+'\n<img src=x onerror="window.__pwned=1"><b>bold?</b>');
 await guest.waitForFunction(()=>document.querySelector('#prompter-view').textContent.includes('<b>bold?</b>'));
 assert.equal(await guest.evaluate(()=>!!document.querySelector('#prompter-view img, #prompter-view b')||!!window.__pwned),false,'script markup was rendered');
 // The guest still can't edit with visibility "everyone".
 await guest.evaluate(()=>window.__sockets.filter(s=>s.readyState===1).at(-1).send(JSON.stringify({t:'prompter_edit',payload:{text:'hijacked'}})));
 await wait(600);
 assert.ok((await host.locator('#prompter-text').inputValue()).startsWith('# Cold open'),'guest edit applied under everyone');

 // Host opens the reader, shares its scroll position and plays.
 await host.locator('#prompter-open-reader').click();
 await host.locator('#prompter-reader').waitFor({state:'visible'});
 await host.locator('#prompter-share').click();
 await guest.locator('#prompter-follow').waitFor({state:'visible'});
 assert.equal(await guest.locator('#prompter-follow').getAttribute('aria-checked'),'true','followers follow by default');
 const fraction=page=>page.evaluate(()=>{const s=document.querySelector('#prompter-scroll');const m=s.scrollHeight-s.clientHeight;return m>0?s.scrollTop/m:0;});
 // Keyboard: arrows nudge a line; Space plays when the reader has focus.
 await host.locator('#prompter-scroll').focus();
 for(let i=0;i<6;i++)await host.keyboard.press('ArrowDown');
 await wait(600);
 const afterNudge=await fraction(host);
 assert.ok(afterNudge>0.02,`arrow keys did not scroll: ${afterNudge}`);
 assert.ok(Math.abs(await fraction(guest)-afterNudge)<0.03,`guest did not follow the nudge: host ${afterNudge}, guest ${await fraction(guest)}`);
 for(let i=0;i<5;i++)await host.locator('[data-act=faster]').click();
 await host.locator('#prompter-scroll').focus();
 await host.keyboard.press('Space');
 assert.equal(await host.locator('#prompter-play').getAttribute('aria-pressed'),'true','Space did not start auto-scroll');
 await guest.waitForFunction(()=>document.querySelector('#prompter-play').getAttribute('aria-pressed')==='true');
 await wait(2500);
 await host.keyboard.press('Space');
 assert.equal(await host.locator('#prompter-play').getAttribute('aria-pressed'),'false');
 await wait(700);
 const [hf,gf]=[await fraction(host),await fraction(guest)];
 assert.ok(hf>afterNudge+0.01,`auto-scroll did not move: ${afterNudge} → ${hf}`);
 assert.ok(Math.abs(hf-gf)<0.03,`follow-host out of sync: host ${hf}, guest ${gf}`);
 console.log('follow host in sync:',JSON.stringify({host:+hf.toFixed(3),guest:+gf.toFixed(3)}));
 await guest.locator('#prompter-follow').evaluate(el=>el.scrollIntoView());
 await screenshot('p5-prompter-reader-390',guest);
 // Mirror and size apply locally.
 await host.locator('#prompter-mirror').click();
 assert.equal(await host.evaluate(()=>getComputedStyle(document.querySelector('#prompter-view')).transform),'matrix(-1, 0, 0, 1, 0, 0)');
 await host.locator('#prompter-mirror').click();
 await host.locator('[data-act=larger]').click();
 await screenshot('p5-prompter-reader-1280',host);
 // Unfollowing keeps the guest where it is.
 await guest.locator('#prompter-follow').click();
 const parked=await fraction(guest);
 await host.locator('#prompter-scroll').focus();
 for(let i=0;i<8;i++)await host.keyboard.press('ArrowDown');
 await wait(600);
 assert.ok(Math.abs(await fraction(guest)-parked)<0.005,'unfollowed guest still moved');

 // Back to private: the guest's reader closes and the entry disappears.
 await host.locator('#prompter-edit').click();
 await host.locator('#prompter-visibility').selectOption('host');
 await guest.waitForFunction(()=>document.querySelector('#prompter-reader').hidden&&document.querySelector('#prompter-btn').hidden);
 assert.equal(await guest.evaluate(()=>document.querySelector('#prompter-view').textContent),'','a hidden script stayed in the guest page');
 console.log('visibility back to host closed the guest reader');

 assert.ok(!logs.includes(MARKER),'script text reached the daemon logs');
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({prompter:'ok',followSync:{host:hf,guest:gf}}));
}catch(e){
 console.error('page errors:',JSON.stringify(errors));
 console.error('daemon logs:',logs.slice(-4000));
 throw e;
}finally{
 await browser?.close();
 if(daemon.exitCode===null){daemon.kill('SIGTERM');await Promise.race([new Promise(r=>daemon.once('exit',r)),wait(3000)]);if(daemon.exitCode===null)daemon.kill('SIGKILL');}
 await rm(data,{recursive:true,force:true});
}
