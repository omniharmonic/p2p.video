import assert from 'node:assert/strict';
import {launchURL,readyToJoin,revealControl} from './lib/harness.mjs';
import {spawn} from 'node:child_process';
import {mkdtemp,mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const dir=await mkdtemp(path.join(tmpdir(),'studiolink-onboarding-')),origin='http://127.0.0.1:17445';
const daemon=spawn(process.env.STUDIO_BINARY,['--data-dir',dir,'--port','17445','--no-funnel','--public-url','https://synthetic-studio.example.ts.net'],{stdio:'ignore'});let browser;
try{
 for(let n=0;n<100;n++){try{if((await fetch(origin+'/health')).ok)break}catch{}await new Promise(r=>setTimeout(r,100))}
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream']});
 const ctx=await browser.newContext({permissions:['camera','microphone','clipboard-read','clipboard-write']});const page=await ctx.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));await page.goto(await launchURL(dir,origin));await page.locator('#home-view').waitFor();
 // The home's help button opens the setup guide [SL-UX-2a].
 await page.locator('#home-help-btn').click();
 await page.getByRole('heading',{name:'How are you using p2p.video?'}).waitFor();
 await page.getByRole('button',{name:/Join someone’s studio/}).click();await page.locator('#wiz-next-btn').click();
 await page.getByRole('heading',{name:'Join with your own account'}).waitFor();
 await page.getByText('Accept the host’s machine share',{exact:true}).waitFor();
 assert.equal(await page.locator('#wiz-copy-serve').count(),0,'guests must not be asked to configure Serve');
 await page.locator('#wiz-prev-btn').click();await page.getByRole('button',{name:/Set up my studio/}).click();await page.locator('#wiz-next-btn').click();
 await page.getByRole('heading',{name:'Give your guest private access'}).waitFor();
 await page.getByText('Share this host with their Tailscale account',{exact:true}).waitFor();
 assert.match(await page.locator('#wizard-body-content').innerText(),/does not prove your guest is connected/);
 await page.getByRole('button',{name:'My second device',exact:true}).click();
 await page.getByText('Connect your second device',{exact:true}).waitFor();
 await page.getByRole('button',{name:'Another person',exact:true}).click();
 assert.match(await page.locator('#wizard-body-content').innerText(),/Send the machine-sharing link/);
 const out=process.env.STUDIO_EVIDENCE_DIR||await mkdtemp(path.join(tmpdir(),'studio-onboarding-evidence-'));await mkdir(out,{recursive:true});await page.screenshot({path:path.join(out,'connection-setup.png'),animations:'disabled'});await page.locator('#wiz-close-btn').click();
 await readyToJoin(page);
 const created=page.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');await page.locator('#name-input').fill('Synthetic host');await page.locator('#join-btn').click();const session=await(await created).json();await page.locator('#studio-view').waitFor({state:'visible'});
 await page.locator('#copy-invite-btn').click();await page.locator('.invite-dialog').waitFor();
 await page.getByText('Share this host in Tailscale first',{exact:true}).waitFor();
 await page.getByRole('button',{name:'My second device',exact:true}).click();
 await page.getByText('Connect your second device',{exact:true}).waitFor();
 await page.getByRole('button',{name:'Another person',exact:true}).click();
 const invite=await page.locator('#guest-invite-value').inputValue();assert.ok(invite.startsWith('https://synthetic-studio.example.ts.net/join?'));assert.ok(!invite.includes(session.hostToken));await page.locator('.invite-dialog').getByRole('button',{name:'Copy meeting link',exact:true}).click();await page.getByText('Meeting link copied.',{exact:true}).waitFor();await page.screenshot({path:path.join(out,'guest-invite.png'),mask:[page.locator('#guest-invite-value')],animations:'disabled'});
 await page.setViewportSize({width:390,height:844});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth));await page.screenshot({path:path.join(out,'guest-invite-mobile.png'),mask:[page.locator('#guest-invite-value')],animations:'disabled'});await page.getByRole('button',{name:'Close invitation'}).click();
 // Reopening setup in an admitted session must not trap users at permissions.
 // In a call the guide lives in the More menu (the header button is hidden).
 await (await revealControl(page,'[data-proxy="wizard-btn"]')).click();await page.locator('#wiz-next-btn').click();await page.locator('#wiz-next-btn').click();await page.getByRole('button',{name:'Check camera and mic',exact:true}).waitFor();await page.getByRole('button',{name:'Check camera and mic',exact:true}).click();assert.deepEqual(errors,[]);
 console.log(JSON.stringify({actionableSetup:true,hostGuestRoles:true,separateMachineShare:true,ownDeviceShortcut:true,guestOnlyCapability:true,copyLink:true,mobileNoOverflow:true,setupAfterJoining:true,errors}));
}finally{await browser?.close();daemon.kill('SIGTERM')}
