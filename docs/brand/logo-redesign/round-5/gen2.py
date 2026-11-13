import os
exec(open("gen.py").read().split("# A —")[0])
# A v2 — bigger crop, puffy domed beret, real bangs
bgA=grad("g","#3a2d63","#1f1838")+'<rect width="1024" height="1024" fill="url(#g)"/>'
faceD='M180 600C180 400 320 300 512 300S844 400 844 600C844 830 700 990 512 990S180 830 180 600Z'
hair=('<path d="M160 620C140 420 290 290 512 284C734 290 884 420 864 620L820 700L800 520L730 570L690 470L620 540L560 450L512 530L450 450L392 540L330 470L290 570L224 520L204 700Z" fill="#4a2f3f"/>')
beret=('<path d="M110 400C110 250 300 150 520 150C750 150 930 250 920 390C910 450 820 470 700 460L330 470C200 470 110 450 110 400Z" fill="#f2b85c"/>'
       '<path d="M150 420C260 470 780 470 900 410C880 460 790 480 700 476L330 486C220 486 160 460 150 420Z" fill="#c9832a"/>'
       '<path d="M500 160Q520 96 580 92Q556 120 552 162Z" fill="#f2b85c"/>'
       '<path d="M260 250Q400 190 560 196" fill="none" stroke="#ffd895" stroke-width="22" stroke-linecap="round"/>')
fgA=(f'<path d="{faceD}" fill="#ffe4cc"/>'+hair+beret
     +eye(386,690,72,94,"#e8a03a")+eye(638,690,72,94,"#e8a03a")
     +mir('<ellipse cx="290" cy="810" rx="50" ry="24" fill="#f59a86"/>')
     +'<path d="M472 840Q512 874 552 840" fill="none" stroke="#1d1424" stroke-width="18" stroke-linecap="round"/>'
     +'<circle cx="736" cy="870" r="22" fill="#f2b85c"/><circle cx="766" cy="846" r="12" fill="#f2b85c"/>')
glA=(f'<path fill-rule="evenodd" d="{faceD}M314 690a72 94 0 1 0 144 0a72 94 0 1 0 -144 0ZM566 690a72 94 0 1 0 144 0a72 94 0 1 0 -144 0Z"/>'
     '<path d="M110 400C110 250 300 150 520 150C750 150 930 250 920 390C910 450 820 470 700 460L330 470C200 470 110 450 110 400Z"/><path d="M500 160Q520 96 580 92Q556 120 552 162Z"/>')
build("apprentice",bgA,fgA,glA)

# B v2 — Kiln: lumpy stone golem mended with glowing gold (kintsugi)
bgB=grad("g","#2b2f5e","#161632")+'<rect width="1024" height="1024" fill="url(#g)"/>'
head='M228 420C200 300 300 216 420 230C470 190 560 186 610 226C730 206 838 290 812 420C870 520 860 700 790 800C720 920 600 960 512 960S300 920 236 800C166 700 166 520 228 420Z'
cracks=('<g fill="none" stroke="#ffcf5a" stroke-width="20" stroke-linecap="round" stroke-linejoin="round">'
        '<path d="M612 228L580 330L640 400L610 470"/><path d="M234 640L320 610L350 660"/><path d="M800 760L720 720"/></g>')
fgB=('<path d="M300 300C262 230 268 160 320 120C330 190 360 230 400 250Z" fill="#8f8aa8"/><path d="M724 300C762 230 756 160 704 120C694 190 664 230 624 250Z" fill="#8f8aa8"/>'+f'<path d="{head}" fill="#8f8aa8"/>'
     '<path d="M300 380Q512 300 724 380" fill="none" stroke="#a9a4c2" stroke-width="26" stroke-linecap="round"/>'
     +cracks
     +mir('<ellipse cx="392" cy="620" rx="74" ry="90" fill="#1d1830"/><ellipse cx="392" cy="620" rx="54" ry="70" fill="#ffc85a"/><ellipse cx="376" cy="594" rx="20" ry="26" fill="#fff6d8"/>')
     +mir('<ellipse cx="300" cy="760" rx="44" ry="20" fill="#c79bb3"/>')
     +'<path d="M474 790Q512 818 550 790" fill="none" stroke="#1d1830" stroke-width="18" stroke-linecap="round"/>'
)
glB=('<path d="M300 300C262 230 268 160 320 120C330 190 360 230 400 250Z"/><path d="M724 300C762 230 756 160 704 120C694 190 664 230 624 250Z"/>'+f'<path fill-rule="evenodd" d="{head}M318 620a74 90 0 1 0 148 0a74 90 0 1 0 -148 0ZM558 620a74 90 0 1 0 148 0a74 90 0 1 0 -148 0Z"/>')
build("kiln",bgB,fgB,glB)

# C v2 — Axolotl: gills fan upward like a crown
bgC=grad("g","#145560","#0b2d36")+'<rect width="1024" height="1024" fill="url(#g)"/>'
def gills(col):
    s=""
    for (x,y,ang,ln) in ((330,470,-115,210),(268,520,-145,220),(236,600,-172,190)):
        s+=f'<g transform="rotate({ang} {x} {y})"><rect x="{x}" y="{y-36}" width="{ln}" height="72" rx="36" fill="{col}"/>'
        s+="".join(f'<circle cx="{x+60+i*50}" cy="{y+34}" r="27" fill="{col}"/>' for i in range(int((ln-50)/50)))+"</g>"
    return s
headC='M512 380C730 380 860 490 860 650C860 830 710 960 512 960S164 830 164 650C164 490 294 380 512 380Z'
fgC=(mir(gills("#ff8f78"))
     +f'<path d="{headC}" fill="#ffd1a1"/>'
     +'<path d="M286 830C380 930 644 930 738 830C700 910 610 960 512 960S324 910 286 830Z" fill="#ffe7cc"/>'
     +eye(376,660,64,78,"#e8a03a")+eye(648,660,64,78,"#e8a03a")
     +mir('<ellipse cx="296" cy="770" rx="48" ry="22" fill="#ff9c86"/>')
     +'<path d="M440 780Q476 814 512 780Q548 814 584 780" fill="none" stroke="#1d1424" stroke-width="18" stroke-linecap="round" stroke-linejoin="round"/>'
     +'<circle cx="512" cy="450" r="20" fill="#ffb98a"/><circle cx="466" cy="470" r="13" fill="#ffb98a"/><circle cx="558" cy="470" r="13" fill="#ffb98a"/>')
glC=(mir(gills("#000"))+f'<path fill-rule="evenodd" d="{headC}M312 660a64 78 0 1 0 128 0a64 78 0 1 0 -128 0ZM584 660a64 78 0 1 0 128 0a64 78 0 1 0 -128 0Z"/>')
build("axolotl",bgC,fgC,glC)
