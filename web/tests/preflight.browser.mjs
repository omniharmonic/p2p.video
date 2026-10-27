// Pre-flight "ready to record" [SL-P0-5] against the real daemon with Chromium
// fake devices (playback muted). Verifies the green-room checklist, the guest's
// checklist reaching the host's aggregate view, the in-call connection path,
// and that a red item on any participant asks the host to confirm before a take
// (Cancel starts nothing; "Record anyway" records and assembles normally).
// Env: STUDIO_BINARY, STUDIO_TEST_PORT (default: free port).
import assert from 'node:assert/strict';
import {launchURL,readyToJoin,revealControl} from './lib/harness.mjs';
import {spawn} from 'node:child_process';
import {mkdtemp,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import net from 'node:net';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const freePort=type=>new Promise((resolve,reject)=>{
 if(type==='udp'){import('node:dgram').then(({createSocket})=>{const s=createSocket('udp4');s.on('error',reject);s.bind(0,'127.0.0.1',()=>{const p=s.address().port;s.close(()=>resolve(p));});});return;}
 const s=net.createServer();s.on('error',reject);s.listen(0,'127.0.0.1',()=>{const p=s.address().port;s.close(()=>resolve(p));});
});
const data=await mkdtemp(path.join(tmpdir(),'studio-preflight-'));
const port=Number(process.env.STUDIO_TEST_PORT||await freePort('tcp')),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel','--udp-port',String(await freePort('udp')),'--turn-listen',`127.0.0.1:${await freePort('tcp')}`],{stdio:['ignore','pipe','pipe']});
let logs='',browser;const errors=[];daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const items=page=>page.evaluate(sel=>Object.fromEntries([...document.querySelectorAll(sel+' .preflight-list li')].map(li=>[li.dataset.preflightId,{status:li.className.replace('pf-',''),detail:li.querySelector('.pf-detail').textContent}])),'#preflight-panel');
try{
 let ready=false;for(let i=0;i<100&&!ready;i++){try{ready=(await fetch(origin+'/health')).ok}catch{}if(!ready)await wait(100);}assert.ok(ready,'daemon did not start');
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const open=async name=>{
  const context=await browser.newContext({permissions:['camera','microphone'],viewport:{width:1280,height:800}});
  const page=await context.newPage();page.setDefaultTimeout(30000);
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
  page.on('console',m=>{if(m.type()==='error'&&!/\b422\b/.test(m.text()))errors.push(`${name} console: ${m.text()}`);});
  return page;
 };
 const host=await open('host');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 // Green room: the checklist runs before joining.
 await host.locator('#preflight-panel').waitFor({state:'visible'});
 // The one-line summary expands to the full checklist [SL-UX-2a].
 await host.locator('#preflight-panel [data-preflight-toggle]').click();
 await host.locator('#preflight-panel [data-preflight-recheck]').click();
 await host.waitForFunction(()=>{const q=id=>document.querySelector(`#preflight-panel li[data-preflight-id="${id}"]`);return q('resolution')?.classList.contains('pf-ok')&&q('mic')&&!q('mic').classList.contains('pf-info');},{},{timeout:15000});
 const green=await items(host);console.log('green room',JSON.stringify(green));
 assert.deepEqual(Object.keys(green),['storage','opfs','persist','camera','mic','resolution','headphones','path','power','recmic']);
 assert.equal(green.recmic.status,'info');assert.match(green.recmic.detail,/^Clean mic \(no headphones\)/,'recording mic mode in the checklist [SL-P1-10]');
 // Headless contexts are incognito with a few GB of quota, so a 2 h take may
 // honestly not fit; the verdict must agree with the numbers it shows.
 const [, freeGB, needGB]=green.storage.detail.match(/^([\d.]+) GB free · 2 h take needs about ([\d.]+) GB/)||[];
 assert.ok(freeGB&&Math.abs(Number(needGB)-10.3)<0.6,green.storage.detail);
 assert.equal(green.storage.status,Number(freeGB)<Number(needGB)?'fail':Number(freeGB)<Number(needGB)*1.25?'warn':'ok',green.storage.detail);
 assert.equal(green.opfs.status,'ok');assert.equal(green.camera.status,'ok',green.camera.detail);
 // Chromium's fake microphone beeps at full scale, so a signal reads as OK or clipping.
 assert.match(green.mic.detail,/Level OK|Clipping/,green.mic.detail);
 assert.equal(green.resolution.status,'ok',green.resolution.detail);assert.match(green.resolution.detail,/1920 × 1080/);
 assert.equal(green.headphones.status,'warn','fake devices are not headphones');
 assert.equal(green.path.status,'info','path is checked after joining');
 assert.ok(['ok','warn','info'].includes(green.persist.status));assert.ok(['ok','warn','info'].includes(green.power.status));
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();await host.locator('#studio-view').waitFor({state:'visible'});
 const guest=await open('guest');
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await guest.locator('#name-input').fill('Synthetic guest');await guest.locator('#join-btn').click();
 // The host sees the waiting guest's checklist before admitting.
 await (await revealControl(host,'#preflight-chip')).click();
 await host.waitForFunction(()=>[...document.querySelectorAll('#preflight-popover .preflight-participant strong')].some(s=>s.textContent==='Synthetic guest'),{},{timeout:20000});
 await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
 for(const p of [host,guest])await p.waitForFunction(()=>document.querySelector('#conn-text').textContent==='Remote video decoded',{},{timeout:20000});
 // In the call: path is classified from the selected candidate pair.
 await (await revealControl(guest,'#preflight-chip')).click();
 await guest.waitForFunction(()=>/Direct|Tailnet/.test(document.querySelector('#preflight-popover li[data-preflight-id="path"] .pf-detail')?.textContent||''),{},{timeout:15000});
 const guestPath=await guest.locator('#preflight-popover li[data-preflight-id="path"] .pf-detail').textContent();console.log('guest path',guestPath);
 await (await revealControl(guest,'#preflight-chip')).click();
 // Guest's browser reports almost no storage: a red item for a 2 h take.
 await guest.evaluate(()=>{navigator.storage.estimate=async()=>({quota:1024**3,usage:0});});
 await (await revealControl(guest,'#preflight-chip')).click();
 await guest.waitForFunction(()=>document.querySelector('#preflight-popover li[data-preflight-id="storage"]')?.classList.contains('pf-fail'));
 await host.waitForFunction(()=>{const s=[...document.querySelectorAll('#preflight-popover .preflight-participant')].find(s=>s.querySelector('strong')?.textContent==='Synthetic guest');return s?.querySelector('li[data-preflight-id="storage"].pf-fail')?.textContent.includes('1.0 GB free');},{},{timeout:15000});
 assert.equal(await host.locator('#preflight-chip').getAttribute('data-overall'),'fail');
 await (await revealControl(host,'#preflight-chip')).click();
 // Record with a red item: confirm lists it; Cancel starts nothing.
 await host.locator('#rec-btn').click();
 await host.locator('#preflight-confirm').waitFor({state:'visible'});
 const problems=await host.locator('#preflight-confirm .preflight-problems').innerText();console.log('confirm',JSON.stringify(problems));
 assert.match(problems,/Synthetic guest · Storage: 1\.0 GB free .*room for about 11 min/);
 await host.locator('#preflight-confirm-cancel').click();
 await wait(2500);
 assert.equal(await guest.locator('#rec-indicator').isVisible(),false,'cancel must not start a take');
 assert.equal(await host.locator('#rec-btn').isEnabled(),true);
 const headers={Authorization:`Bearer ${session.hostToken}`};
 assert.equal((await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json()).takes?.length??0,0);
 // "Record anyway" records normally.
 await host.locator('#rec-btn').click();await host.locator('#preflight-confirm-record').click();
 for(const p of [host,guest])await p.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 await wait(2500);await host.locator('#rec-btn').click();
 let snapshot;for(let i=0;i<300;i++){const d=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json();const take=d.takes?.[0];if(take){snapshot=await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers})).json();if(snapshot.tracks.length===2&&snapshot.tracks.every(t=>t.status==='assembled'))break;}await wait(100);}
 assert.equal(snapshot.tracks.length,2);assert.ok(snapshot.tracks.every(t=>t.status==='assembled'));
 // Layout: the popover fits a phone viewport.
 await host.locator('#transfers-recovery-modal').waitFor({state:'visible'});await host.locator('#close-transfers-btn').click();
 await host.setViewportSize({width:390,height:844});await (await revealControl(host,'#preflight-chip')).click();
 assert.ok(await host.evaluate(()=>{const r=document.querySelector('#preflight-popover').getBoundingClientRect();return r.left>=0&&r.right<=window.innerWidth&&document.documentElement.scrollWidth<=window.innerWidth;}),'popover overflows at 390px');
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({greenRoomChecklist:true,hostSeesGuestChecklist:true,guestPath,confirmOnRed:true,cancelStartsNothing:true,recordAnywayAssembled:snapshot.tracks.length,errors}));
}catch(e){console.error('errors',JSON.stringify(errors));console.error(logs.slice(-4000));throw e;}
finally{await browser?.close();daemon.kill('SIGTERM');await Promise.race([new Promise(r=>daemon.once('exit',r)),wait(3000)]);await rm(data,{recursive:true,force:true});}
