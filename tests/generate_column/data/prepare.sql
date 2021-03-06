drop database if exists `generate_column`;
create database `generate_column`;
use `generate_column`;

create table t (a int, b int as (a + 1) stored primary key);
insert into t(a) values (1),(2), (3),(4),(5),(6),(7);
update t set a = 10 where a = 1;
update t set a = 11 where b = 3;
delete from t where b=4;
delete from t where a=4;

create table t1 (a int, b int as (a + 1) virtual not null, unique index idx(b));
insert into t1 (a) values (1),(2), (3),(4),(5),(6),(7);
update t1 set a = 10 where a = 1;
update t1 set a = 11 where b = 3;
delete from t1 where b=4;
delete from t1 where a=4;
