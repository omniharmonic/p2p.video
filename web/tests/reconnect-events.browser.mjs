// Signaling recovers after the browser goes offline and comes back. The
// 'online'/visibility listeners only shorten recovery (backoff is capped at
// 3 s and reconnects regardless), so this guards the outcome, plus that the
// wake-up listeners are registered at all. A mutation that removes only the
// signaling 'online' listener is NOT detected: other modules also listen for
// 'online'. Fake devices, no audible playback.
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdtemp} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {freePort,isolatedNetworkArgs,launchURL,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');

const data=await mkdtemp(path.join(tmpdir(),'studio-reconnect-events-'));
const port=Number(process.env.STUDIO_TEST_PORT||await freePort('tcp')),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:'ignore'});
let browser;
try{
 for(let n=0;n<100;n++){try{if((await fetch(origin+'/health')).ok)break}catch{}await new Promise(r=>setTimeout(r,100));}
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--mute-audio']});
 const context=await browser.newContext({permissions:['camera','microphone']});
 await context.addInitScript(()=>{
  // Record listener registrations and every socket the page opens.
  window.__listeners=[];
  for(const target of [window,document]){
   const add=target.addEventListener.bind(target);
   target.addEventListener=(type,fn,opts)=>{window.__listeners.push((target===window?'window:':'document:')+type);return add(type,fn,opts);};
  }
  const Original=window.WebSocket;window.__sockets=[];
  window.WebSocket=class extends Original{constructor(...args){super(...args);window.__sockets.push(this);}};
 });
 const host=await context.newPage();host.setDefaultTimeout(30000);
 await host.goto(await launchURL(data,origin));
 await readyToJoin(host);await host.locator('#name-input').fill('Host');await host.locator('#join-btn').click();
 await host.locator('#studio-view').waitFor({state:'visible'});
 const registered=await host.evaluate(()=>window.__listeners);
 for(const want of ['window:online','document:visibilitychange','window:beforeunload'])
  assert.ok(registered.includes(want),`missing reconnect listener ${want}: ${registered.join(',')}`);

 // Fake an offline period: block new sockets, drop the live one, then let
 // 'online' wake signaling. It must reconnect without waiting for a reload.
 const before=await host.evaluate(()=>window.__sockets.length);
 await context.setOffline(true);
 await host.evaluate(()=>window.__sockets.at(-1).close(4001,'test offline'));
 await new Promise(r=>setTimeout(r,1500));
 await context.setOffline(false);
 await host.evaluate(()=>window.dispatchEvent(new Event('online')));
 await host.waitForFunction(n=>window.__sockets.length>n&&window.__sockets.at(-1).readyState===WebSocket.OPEN,before,{timeout:10000});
 await host.waitForFunction(()=>document.querySelector('#studio-view').offsetParent!==null);
 console.log(JSON.stringify({wakeListenersRegistered:true,recoveredAfterOffline:true}));
}finally{
 await browser?.close();daemon.kill('SIGTERM');
}
