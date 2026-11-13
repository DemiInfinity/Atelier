import os
H='<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024">'
def w(d,n,body):
    os.makedirs(d,exist_ok=True); open(f"{d}/{n}.svg","w").write(H+body+"</svg>")
def grad(id,a,b): return f'<defs><linearGradient id="{id}" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="{a}"/><stop offset="1" stop-color="{b}"/></linearGradient></defs>'
def mir(s): return s+f'<g transform="translate(1024,0) scale(-1,1)">{s}</g>'
INK="#1d1424"
def eye(x,y,rx,ry,iris,flip=False):
    hx = x-rx*0.3
    return (f'<ellipse cx="{x}" cy="{y}" rx="{rx}" ry="{ry}" fill="{INK}"/>'
      f'<ellipse cx="{x}" cy="{y+ry*0.42}" rx="{rx*0.7}" ry="{ry*0.42}" fill="{iris}"/>'
      f'<circle cx="{hx}" cy="{y-ry*0.38}" r="{rx*0.42}" fill="#fff"/><circle cx="{x+rx*0.38}" cy="{y+ry*0.2}" r="{rx*0.17}" fill="#fff"/>')
def build(name,bg,fg,glyph):
    d=f"concepts/{name}"; w(d,"bg",bg); w(d,"fg",fg); w(d,"glyph",glyph); w(d,"icon",bg+fg)

# A — Apprentice: anime artisan under a big amber beret, face fills the tile
bgA=grad("g","#3a2d63","#1f1838")+'<rect width="1024" height="1024" fill="url(#g)"/>'
face='<path d="M232 560C232 380 360 300 512 300S792 380 792 560C792 760 670 900 512 900S232 760 232 560Z"'
hair=('<path d="M220 560C200 400 300 270 512 262C724 270 824 400 804 560L770 620L752 470L690 520L640 430L580 500L512 420L444 500L384 430L334 520L272 470L254 620Z" fill="#4a2f3f"/>')
beret=('<ellipse cx="500" cy="330" rx="380" ry="150" transform="rotate(-9 500 330)" fill="#f2b85c"/>'
       '<ellipse cx="500" cy="360" rx="330" ry="70" transform="rotate(-9 500 360)" fill="#d9952f"/>'
       '<path d="M560 196Q590 150 640 160Q612 180 600 214Z" fill="#f2b85c"/>')
fgA=(f'{face} fill="#ffe4cc"/>'+hair+beret
     +eye(400,640,64,84,"#e8a03a")+eye(624,640,64,84,"#e8a03a")
     +mir('<ellipse cx="330" cy="740" rx="44" ry="22" fill="#f59a86"/>')
     +'<path d="M478 770Q512 800 546 770" fill="none" stroke="#1d1424" stroke-width="16" stroke-linecap="round"/>'
     +'<circle cx="700" cy="790" r="20" fill="#f2b85c"/><circle cx="726" cy="770" r="11" fill="#f2b85c"/>')
glA=('<path fill-rule="evenodd" d="M232 560C232 380 360 300 512 300S792 380 792 560C792 760 670 900 512 900S232 760 232 560Z'
     'M336 640a64 84 0 1 0 128 0a64 84 0 1 0 -128 0ZM560 640a64 84 0 1 0 128 0a64 84 0 1 0 -128 0Z"/>'
     '<ellipse cx="500" cy="330" rx="380" ry="150" transform="rotate(-9 500 330)"/><path d="M560 196Q590 150 640 160Q612 180 600 214Z"/>')
build("apprentice",bgA,fgA,glA)

# B — Kiln: a little clay golem with glowing eyes and a branching rune
bgB=grad("g","#2b2f5e","#161632")+'<rect width="1024" height="1024" fill="url(#g)"/>'
head='M200 330C200 250 260 210 340 214L700 206C780 204 830 250 830 330L846 760C848 860 780 920 690 920H334C244 920 180 860 182 760Z'
rune=('<g fill="none" stroke="#ffd27a" stroke-width="30" stroke-linecap="round" stroke-linejoin="round"><path d="M512 270V420M512 400Q512 340 572 320"/></g>'
      '<g fill="#ffd27a"><circle cx="512" cy="270" r="24"/><circle cx="512" cy="430" r="24"/><circle cx="584" cy="316" r="22"/></g>')
fgB=(f'<path d="{head}" fill="#c9714c"/>'
     '<path d="M200 330C200 250 260 210 340 214L700 206C780 204 830 250 830 330L834 420C700 380 330 380 196 430Z" fill="#d98a5e"/>'
     +rune
     +mir('<ellipse cx="380" cy="590" rx="78" ry="96" fill="#2a1a1a"/><ellipse cx="380" cy="590" rx="58" ry="76" fill="#ffc85a"/><ellipse cx="364" cy="562" rx="22" ry="28" fill="#fff6d8"/>')
     +mir('<ellipse cx="300" cy="730" rx="46" ry="22" fill="#e0866a"/>')
     +'<path d="M470 760Q512 790 554 760" fill="none" stroke="#2a1a1a" stroke-width="18" stroke-linecap="round"/>'
     +'<path d="M720 820q30 -10 44 -40" fill="none" stroke="#a55a3a" stroke-width="14" stroke-linecap="round"/><path d="M262 300q20 -26 56 -30" fill="none" stroke="#e9a37a" stroke-width="14" stroke-linecap="round"/>')
glB=(f'<path fill-rule="evenodd" d="{head}M302 590a78 96 0 1 0 156 0a78 96 0 1 0 -156 0ZM566 590a78 96 0 1 0 156 0a78 96 0 1 0 -156 0Z"/>')
build("kiln",bgB,fgB,glB)

# C — Axolotl familiar with a frilly gill crown
bgC=grad("g","#145560","#0b2d36")+'<rect width="1024" height="1024" fill="url(#g)"/>'
def gills(col):
    s=""
    for (x,y,ang,ln) in ((250,470,-58,230),(214,560,-22,240),(220,660,14,220)):
        s+=f'<g transform="rotate({ang} {x} {y})"><rect x="{x-ln}" y="{y-34}" width="{ln}" height="68" rx="34" fill="{col}"/>'
        s+="".join(f'<circle cx="{x-ln+40+i*52}" cy="{y-30}" r="26" fill="{col}"/>' for i in range(int((ln-40)/52)))+"</g>"
    return s
headC='M512 340C720 340 840 450 840 610C840 770 700 880 512 880S184 770 184 610C184 450 304 340 512 340Z'
fgC=(mir(gills("#ff8f78"))+mir(gills.__call__("#ff8f78").replace("#ff8f78","#ff8f78"))[:0]
     +f'<path d="{headC}" fill="#ffd1a1"/>'
     +'<path d="M300 760C380 860 640 860 724 760C700 830 610 880 512 880S330 830 300 760Z" fill="#ffe7cc"/>'
     +eye(380,600,58,70,"#e8a03a")+eye(644,600,58,70,"#e8a03a")
     +mir('<ellipse cx="310" cy="700" rx="44" ry="22" fill="#ff9c86"/>')
     +'<path d="M440 712Q476 744 512 712Q548 744 584 712" fill="none" stroke="#1d1424" stroke-width="16" stroke-linecap="round" stroke-linejoin="round"/>'
     +'<circle cx="512" cy="400" r="18" fill="#ffb98a"/><circle cx="470" cy="420" r="12" fill="#ffb98a"/><circle cx="554" cy="420" r="12" fill="#ffb98a"/>')
glC=(mir(gills("#000"))+f'<path fill-rule="evenodd" d="{headC}M322 600a58 70 0 1 0 116 0a58 70 0 1 0 -116 0ZM586 600a58 70 0 1 0 116 0a58 70 0 1 0 -116 0Z"/>')
build("axolotl",bgC,fgC,glC)
