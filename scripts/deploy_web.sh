nvm use v20.15.1
cd web && npm ci && npm run build && cd ..
sudo rm -rf /usr/share/saturnus/web/*
sudo cp -a web/dist/. /usr/share/saturnus/web/

