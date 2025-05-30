# DistroTarea2

## Gonzalo Alarcón 202173646-8
## Esteban Gárate 202173625-5

---

- En dist121 se encuentra LCP.
- En dist122 se encuentra Entrenador y CDP.
- En dist123 se encuentra Gimnasios. 
- En dist124 se encuentra SNP

Dentro de cada máquina, entrar a la carpeta DISTROTAREA2.
```
cd DistroTarea2/
```

Luego ejecutar las máquinas en el siguiente orden y utilizando los siguientes comandos:

1. En dist123
```
make docker-gimnasio
```

2. En dist121
```
make docker-lcp
```

3. En dist124
```
make docker-snp
```

4. En dist122
```
sudo docker-compose down
make docker-entrenadores-cdp
```

---

### Menu Entrenador

- Con 1 se ven los torenos disponibles
- Con 2 se inscribe a un toreno
- Con 3 se ven los datos
- Con 4 se sale