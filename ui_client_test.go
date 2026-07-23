package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// domShim is a hand-rolled, dependency-free DOM just large enough for morph:
// element/text nodes, a live attribute list, and child insertion/removal. It
// lets the client script be exercised under plain node with no npm install, so
// `go test ./...` stays the whole test story (CI installs Go only).
const domShim = `
function Text(v){return {nodeType:3,nodeValue:v,parentNode:null};}
function El(tag,attrs,kids){
 var e={nodeType:1,tagName:tag,childNodes:[],parentNode:null,_a:{}};
 for(var k in (attrs||{}))e._a[k]=String(attrs[k]);
 e.getAttribute=function(n){return n in this._a?this._a[n]:null;};
 e.setAttribute=function(n,v){this._a[n]=String(v);};
 e.removeAttribute=function(n){delete this._a[n];};
 e.hasAttribute=function(n){return n in this._a;};
 e.insertBefore=function(c,ref){
  if(c.parentNode){var p=c.parentNode;p.childNodes.splice(p.childNodes.indexOf(c),1);}
  c.parentNode=this;
  var i=ref?this.childNodes.indexOf(ref):-1;
  if(i<0)i=this.childNodes.length;
  this.childNodes.splice(i,0,c);
  return c;};
 e.appendChild=function(c){return this.insertBefore(c,null);};
 e.removeChild=function(c){var i=this.childNodes.indexOf(c);if(i>=0){this.childNodes.splice(i,1);c.parentNode=null;}return c;};
 Object.defineProperty(e,'attributes',{get:function(){var o=[];for(var k in this._a)o.push({name:k,value:this._a[k]});return o;}});
 (kids||[]).forEach(function(c){e.appendChild(c);});
 return e;
}
function fail(msg){console.error('FAIL: '+msg);process.exitCode=1;}
function assert(cond,msg){if(!cond)fail(msg);}
`

// runJS writes the dashboard script plus a test body to a temp dir and executes
// it under node. Any "FAIL:" the body prints becomes a Go test failure.
func runJS(t *testing.T, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping client-script test")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "ui.js")
	if err := os.WriteFile(src, []byte(uiJS+"\n"+domShim+"\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, src).CombinedOutput()
	if err != nil {
		t.Fatalf("node run failed: %v\n%s", err, out)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Errorf("client script assertions failed:\n%s", s)
	}
}

// TestPollAppendsTranscriptWithoutRebuildingPane is the regression guard for the
// "whole screen reloads on every transcript update" bug. Assigning innerHTML
// replaced the entire detail pane, so one appended transcript line re-created
// every node — replaying the .fadein entrance animation across the screen and
// dropping selection, focus and scroll offsets. Reusing the live nodes is the
// property under test: node identity must survive the update.
func TestPollAppendsTranscriptWithoutRebuildingPane(t *testing.T) {
	runJS(t, `
var row0=El('div',{},[Text('Bash ls')]),row1=El('div',{},[Text('ok')]);
var feed=El('div',{'class':'txfeed scroll','data-seq':'1'},[row0,row1]);
var cost=Text('$0.12');
var card=El('div',{'class':'fadein rounded-md'},[El('span',{'class':'cost'},[cost]),feed]);
var li=El('li',{'data-k':'s1','class':'relative fadein'},[card]);
var live=El('main',{},[li]);

// The next poll differs only by one appended transcript row and a new cost.
var next=El('div',{},[
 El('li',{'data-k':'s1','class':'relative fadein'},[
  El('div',{'class':'fadein rounded-md'},[
   El('span',{'class':'cost'},[Text('$0.14')]),
   El('div',{'class':'txfeed scroll','data-seq':'1'},[
    El('div',{},[Text('Bash ls')]),
    El('div',{},[Text('ok')]),
    El('div',{},[Text('Edit serve.go')])])])])]);

morph(live,next);

assert(live.childNodes[0]===li,'step <li> was re-created instead of patched');
assert(li.childNodes[0]===card,'step card was re-created; .fadein would replay');
assert(card.childNodes[1]===feed,'transcript feed was re-created; scroll offset would reset');
assert(feed.childNodes.length===3,'expected 3 transcript rows, got '+feed.childNodes.length);
assert(feed.childNodes[0]===row0,'transcript row 0 was re-created instead of kept');
assert(feed.childNodes[1]===row1,'transcript row 1 was re-created instead of kept');
assert(feed.childNodes[2].childNodes[0].nodeValue==='Edit serve.go','new transcript row not appended');
assert(cost.nodeValue==='$0.14','changed cost was not patched in place');
`)
}

// TestPollKeepsUserToggledDisclosure covers the other half of the flicker: a
// running step's fragment always carries <details open>, so re-applying server
// attributes wholesale would re-open a disclosure the user just closed.
func TestPollKeepsUserToggledDisclosure(t *testing.T) {
	runJS(t, `
var d=El('details',{'data-disc':'1-transcript','class':'group'});
var live=El('main',{},[d]);
var next=El('div',{},[El('details',{'data-disc':'1-transcript','class':'group','open':''})]);
morph(live,next);
assert(live.childNodes[0]===d,'details element was re-created');
assert(!d.hasAttribute('open'),'server fragment re-opened a disclosure the user closed');

// The reverse must also hold: a disclosure the user opened stays open when the
// server stops emitting the attribute.
var d2=El('details',{'data-disc':'2-prompt','open':''});
morph(El('main',{},[d2]),El('div',{},[El('details',{'data-disc':'2-prompt'})]));
assert(d2.hasAttribute('open'),'server fragment closed a disclosure the user opened');
`)
}

// TestMorphReordersKeyedNodes checks the rail case: tickets can change order as
// their state labels change, and a keyed node must move rather than be rebuilt.
func TestMorphReordersKeyedNodes(t *testing.T) {
	runJS(t, `
var a=El('a',{'data-k':'t1'}),b=El('a',{'data-k':'t2'});
var live=El('nav',{},[a,b]);
morph(live,El('div',{},[El('a',{'data-k':'t2'}),El('a',{'data-k':'t3'}),El('a',{'data-k':'t1'})]));
assert(live.childNodes.length===3,'expected 3 rail entries, got '+live.childNodes.length);
assert(live.childNodes[0]===b,'keyed node t2 was rebuilt instead of moved');
assert(live.childNodes[2]===a,'keyed node t1 was rebuilt instead of moved');
assert(live.childNodes[1].getAttribute('data-k')==='t3','new rail entry not inserted');
`)
}

// TestMorphSwapsActionButtonsInsteadOfRelabelling guards the stop/continue pair.
// act() disables the button it fired as a JS property, which no server fragment
// can clear. If morph reused the stop button's node for the continue button that
// replaces it, the freshly rendered continue button would come up permanently
// disabled — hence the distinct data-k keys on the two buttons.
func TestMorphSwapsActionButtonsInsteadOfRelabelling(t *testing.T) {
	runJS(t, `
var stop=El('button',{'data-k':'act-stop','data-act':'stop','data-issue':'7'});
stop.disabled=true; // what act() does the moment the user clicks it
var live=El('div',{},[El('a',{}),stop]);
morph(live,El('div',{},[El('a',{}),El('button',{'data-k':'act-continue','data-act':'continue','data-issue':'7'})]));
assert(live.childNodes.length===2,'expected the link plus one button, got '+live.childNodes.length);
var btn=live.childNodes[1];
assert(btn!==stop,'stop button was re-labelled instead of swapped; it stays disabled');
assert(btn.getAttribute('data-act')==='continue','continue button not rendered');
assert(!btn.disabled,'the new continue button inherited the disabled state');
`)
}

// TestMorphRemovesDroppedNodes guards the shrink direction.
func TestMorphRemovesDroppedNodes(t *testing.T) {
	runJS(t, `
var live=El('div',{},[El('p',{'data-k':'a'}),El('p',{'data-k':'b'}),El('p',{'data-k':'c'})]);
morph(live,El('div',{},[El('p',{'data-k':'a'})]));
assert(live.childNodes.length===1,'stale nodes were not removed, got '+live.childNodes.length);
assert(live.childNodes[0].getAttribute('data-k')==='a','wrong node survived');
`)
}
